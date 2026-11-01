package sheets

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

type Writer interface {
	Replace(context.Context, service.ExportTable) error
}
type Worker struct {
	Service     *service.Service
	Writer      Writer
	Interval    time.Duration
	Attempts    *prometheus.CounterVec
	LastSuccess prometheus.Gauge
}

// Sync holds a database-wide transaction lock through the bounded HTTP write.
// Each attempt reads current state; no in-memory cursor can lose a DB change.
func (w *Worker) Sync(ctx context.Context) (bool, error) {
	tx, err := w.Service.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, store.Q("sheets_sync_lock")).Scan(&locked); err != nil || !locked {
		return false, err
	}
	table, err := w.Service.ExportTable(ctx, tx)
	if err != nil {
		return false, err
	}
	if err = w.Writer.Replace(ctx, table); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval < 10*time.Second {
		interval = time.Minute
	}
	// Wait after attempts, so failures cannot cause a tight retry loop.
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 45*time.Second)
		started := time.Now()
		written, err := w.Sync(attempt)
		cancel()
		result := "skipped"
		if err != nil {
			result = "failed"
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "sheets.sync_failed")
			}
		} else if written {
			result = "success"
			if w.LastSuccess != nil {
				w.LastSuccess.SetToCurrentTime()
			}
			slog.InfoContext(ctx, "sheets.synced", "duration_ms", time.Since(started).Milliseconds())
		}
		if w.Attempts != nil {
			w.Attempts.WithLabelValues(result).Inc()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
