package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/queue"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func Auth(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "authentication required")
		}
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return next(bounded, req)
	}
}
func Serve(ctx context.Context, c Config) error {
	start, cancel := context.WithTimeout(ctx, 10*time.Second)
	db, err := store.Open(start, c.DSN)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	if err = store.CheckSchema(ctx, db); err != nil {
		return err
	}
	svc := &service.Service{DB: db, RootID: c.RootID, BotID: c.BotID, Milestones: c.Milestones, ParticipationEnabled: c.ParticipationEnabled}
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	calls := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registration_rpc_total", Help: "RPC results"}, []string{"method", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registration_rpc_seconds", Help: "RPC latency"}, []string{"method"})
	registry.MustRegister(calls, duration)
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(64 << 10), grpc.MaxSendMsgSize(16 << 20), grpc.MaxConcurrentStreams(32), grpc.StatsHandler(otelgrpc.NewServerHandler()), grpc.ChainUnaryInterceptor(Auth(c.Token), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		t := time.Now()
		res, e := next(ctx, req)
		calls.WithLabelValues(info.FullMethod, status.Code(e).String()).Inc()
		duration.WithLabelValues(info.FullMethod).Observe(time.Since(t).Seconds())
		if e != nil {
			slog.WarnContext(ctx, "RPC failed", "method", info.FullMethod, "code", status.Code(e).String())
		}
		return res, e
	})}
	if c.TLSCert != "" {
		creds, e := credentials.NewServerTLSFromFile(c.TLSCert, c.TLSKey)
		if e != nil {
			return errors.New("cannot load gRPC TLS")
		}
		options = append(options, grpc.Creds(creds))
	}
	server := grpc.NewServer(options...)
	pb.RegisterRegistrationServer(server, svc)
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return errors.New("cannot bind gRPC")
	}
	defer listener.Close()
	queueSize := prometheus.NewGauge(prometheus.GaugeOpts{Name: "registration_outbound_pending", Help: "Pending messages"})
	queueAge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "registration_outbound_oldest_seconds", Help: "Oldest pending message age"})
	registry.MustRegister(queueSize, queueAge)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if db.Ping(ctx) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	httpServer := &http.Server{Addr: c.HTTP, Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	run, stop := context.WithCancel(ctx)
	defer stop()
	var workers sync.WaitGroup
	writer := &kafka.Writer{Addr: kafka.TCP(c.Brokers...), Balancer: &kafka.Hash{}, RequiredAcks: kafka.RequireAll, Async: false, WriteTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, MaxAttempts: 3, AllowAutoTopicCreation: false}
	workers.Go(func() {
		defer writer.Close()
		(&queue.Publisher{DB: db, Writer: writer, Prefix: c.TopicPrefix}).Run(run)
	})
	workers.Go(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(run, 3*time.Second)
				var count int64
				var age float64
				if db.QueryRow(ctx, store.Q("queue_metrics")).Scan(&count, &age) == nil {
					queueSize.Set(float64(count))
					queueAge.Set(age)
				}
				cancel()
			}
		}
	})
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.Serve(listener) }()
	go func() { errorsCh <- httpServer.ListenAndServe() }()
	slog.Info("backend started")
	select {
	case <-ctx.Done():
	case <-errorsCh:
		err = errors.New("server stopped unexpectedly")
	}
	stop()
	done := make(chan struct{})
	go func() { server.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		server.Stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	workers.Wait()
	return err
}
