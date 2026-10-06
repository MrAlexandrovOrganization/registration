package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestObserveRejectedRPCWithoutPayload(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	calls := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_rpc_total", Help: "test"}, []string{"method", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_rpc_seconds", Help: "test"}, []string{"method"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(calls, duration)
	info := &grpc.UnaryServerInfo{FullMethod: "/registration.v1.Registration/Accept"}
	_, err := ObserveRPC(calls, duration)(context.Background(), "synthetic-private-payload", info, func(ctx context.Context, req any) (any, error) {
		return Auth("synthetic-secret")(ctx, req, info, func(context.Context, any) (any, error) {
			t.Fatal("unauthenticated handler called")
			return nil, nil
		})
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("auth bypassed")
	}
	if strings.Contains(logs.String(), "synthetic") || !strings.Contains(logs.String(), "Unauthenticated") || !strings.Contains(logs.String(), "duration_ms") {
		t.Fatal("unsafe or missing RPC log")
	}
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.GetName() == "test_rpc_total" && metric.Metric[0].GetCounter().GetValue() == 1 {
			return
		}
	}
	t.Fatal("rejected request not counted")
}
