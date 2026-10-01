package queue

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"registration.local/backend/internal/store"
)

type Publisher struct {
	DB     *pgxpool.Pool
	Writer *kafka.Writer
	Prefix string
}

func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := p.Once(run)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "outbox publication deferred")
			}
		}
	}
}
func (p *Publisher) Once(ctx context.Context) error {
	if _, err := p.DB.Exec(ctx, store.Q("reconcile_cancelled")); err != nil {
		return err
	}
	rows, err := p.DB.Query(ctx, store.Q("publish_due"))
	if err != nil {
		return err
	}
	type item struct {
		id, chat              int64
		priority, traceparent string
	}
	var batch []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.chat, &i.priority, &i.traceparent); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, i := range batch {
		msg := kafka.Message{Topic: p.Prefix + "." + i.priority + ".v1", Key: []byte(strconv.FormatInt(i.chat, 10)), Value: []byte(strconv.FormatInt(i.id, 10)), Headers: []kafka.Header{{Key: "traceparent", Value: []byte(i.traceparent)}}}
		if err = p.Writer.WriteMessages(ctx, msg); err != nil {
			return err
		}
		if _, err = p.DB.Exec(ctx, store.Q("published"), i.id); err != nil {
			return err
		}
	}
	return nil
}
