package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"registration.local/backend/internal/app"
	"registration.local/backend/internal/observability"
)

func main() { os.Exit(run()) }
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	shutdown, err := observability.Init(ctx, "registration-backend")
	if err != nil {
		slog.Error("telemetry initialization failed")
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	if err = app.Run(ctx, os.Args[1:]); err != nil {
		slog.Error("command failed", "reason", err.Error())
		return 1
	}
	return 0
}
