//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/app"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

// Baseline for real gRPC + PostgreSQL Accept, without delivery, Kafka or export.
// Use only via scripts/benchmark.py, which provisions a disposable database.
func BenchmarkAccept(b *testing.B) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	defer slog.SetDefault(old)
	for _, concurrency := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("clients=%d", concurrency), func(b *testing.B) {
			cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
			if err != nil || cfg.ConnConfig.Database != "registration_benchmark" || cfg.ConnConfig.Host != "127.0.0.1" {
				b.Fatal("requires disposable loopback registration_benchmark database")
			}
			db, err := store.Open(b.Context(), os.Getenv("TEST_DATABASE_URL"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := store.Migrate(b.Context(), db); err != nil {
				b.Fatal(err)
			}
			// This is fixture-only setup, outside measured time, for each calibration.
			if _, err := db.Exec(b.Context(), "TRUNCATE users,processed_updates,outbound_messages,first_starts CASCADE"); err != nil {
				b.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			calls := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bench_rpc", Help: "fixture"}, []string{"method", "code"})
			duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bench_duration", Help: "fixture"}, []string{"method"})
			server := grpc.NewServer(grpc.MaxRecvMsgSize(64<<10), grpc.MaxConcurrentStreams(32), grpc.ChainUnaryInterceptor(app.ObserveRPC(calls, duration), app.Auth(strings.Repeat("x", 32))))
			pb.RegisterRegistrationServer(server, &service.Service{DB: db, BotID: 1000})
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			client := pb.NewRegistrationClient(conn)
			ctx := metadata.AppendToOutgoingContext(b.Context(), "authorization", "Bearer "+strings.Repeat("x", 32))
			if _, err := client.PendingInteractive(ctx, &pb.PendingInteractiveRequest{}); err != nil {
				b.Fatal(err)
			}
			latency := make([]time.Duration, b.N)
			var next, failures atomic.Int64
			var workers sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for range concurrency {
				workers.Go(func() {
					for {
						i := next.Add(1) - 1
						if i >= int64(b.N) {
							return
						}
						t := time.Now()
						call, cancel := context.WithTimeout(ctx, 15*time.Second)
						r, err := client.Accept(call, &pb.Update{Id: i + 1, Actor: i + 1, Chat: i + 1, ChatType: "private", Kind: "message", Text: "/start"})
						cancel()
						latency[i] = time.Since(t)
						if err != nil || len(r.GetDeliveryIds()) != 2 || r.GetDuplicate() {
							failures.Add(1)
						}
					}
				})
			}
			workers.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			slices.Sort(latency)
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "requests/s")
			b.ReportMetric(float64(latency[(len(latency)-1)*95/100])/float64(time.Millisecond), "p95-ms")
			b.ReportMetric(float64(latency[(len(latency)-1)*99/100])/float64(time.Millisecond), "p99-ms")
			b.ReportMetric(float64(failures.Load()), "errors")
			if failures.Load() != 0 {
				b.Fatal("Accept failures under load")
			}
		})
	}
}
