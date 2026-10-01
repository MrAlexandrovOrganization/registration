package service

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func (s *Service) Claim(ctx context.Context, r *pb.ClaimRequest) (*pb.Delivery, error) {
	if len(r.Worker) < 8 || len(r.Worker) > 100 || r.Id < 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid claim")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	var cooldownMillis int64
	err = tx.QueryRow(ctx, store.Q("sender_lock"), r.Worker).Scan(&cooldownMillis)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.ResourceExhausted, "another sender owns this bot")
	}
	if err != nil {
		return nil, internal(err)
	}
	d := &pb.Delivery{}
	if cooldownMillis > 0 {
		d.NotBeforeUnixMs = time.Now().Add(time.Duration(cooldownMillis) * time.Millisecond).UnixMilli()
	}
	if r.Id > 0 && cooldownMillis == 0 {
		d.Id = r.Id
		d.Lease = token()
		var body []byte
		err = tx.QueryRow(ctx, store.Q("claim"), r.Id, d.Lease).Scan(&d.Chat, &d.Kind, &body, &d.SourceChat, &d.SourceMessage, &d.Actor, &d.Traceparent, &d.Group)
		if errors.Is(err, pgx.ErrNoRows) {
			d.Id = 0
			d.Lease = ""
		} else if err != nil {
			return nil, internal(err)
		} else {
			d.View = &pb.View{}
			if err = protojson.Unmarshal(body, d.View); err != nil {
				return nil, status.Error(codes.Internal, "invalid persisted view")
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return d, nil
}

// RetryPolicy separates rate-limit waiting from failed delivery attempts.
func RetryPolicy(outcome string, attempts int, retryAfter int64) (state string, increment int, delay int64, code string) {
	switch outcome {
	case "sent":
		return "sent", 0, 0, ""
	case "blocked":
		return "failed", 1, 0, "blocked"
	case "permanent":
		return "failed", 1, 0, "content"
	case "cancelled":
		return "cancelled", 0, 0, ""
	case "rate_limit":
		return "retry_wait", 0, max(1, min(retryAfter, 604800)), "rate_limit"
	default:
		if attempts >= 7 {
			return "failed", 1, 0, "exhausted"
		}
		delay = int64(1<<min(attempts+1, 9)) + rand.Int64N(3)
		code = "transient"
		if outcome == "uncertain" {
			code = "uncertain"
		}
		return "retry_wait", 1, delay, code
	}
}
func (s *Service) Complete(ctx context.Context, r *pb.Completion) (*pb.Receipt, error) {
	if r.Id <= 0 || r.Lease == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid completion")
	}
	switch r.Outcome {
	case "sent", "blocked", "permanent", "transient", "uncertain", "rate_limit", "cancelled":
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid outcome")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	var chat, actor int64
	var attempts int
	var broadcast *int64
	var kind string
	var body []byte
	err = tx.QueryRow(ctx, store.Q("completion_lock"), r.Id, r.Lease).Scan(&chat, &attempts, &broadcast, &kind, &body, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.FailedPrecondition, "lease expired or already completed")
	}
	if err != nil {
		return nil, internal(err)
	}
	state, increment, delay, code := RetryPolicy(r.Outcome, attempts, r.RetryAfterSeconds)
	if _, err = tx.Exec(ctx, store.Q("complete"), r.Id, r.Lease, state, increment, float64(delay), r.TelegramMessageId, code); err != nil {
		return nil, internal(err)
	}
	if r.Outcome == "rate_limit" {
		if _, err = tx.Exec(ctx, store.Q("cooldown"), float64(delay)); err != nil {
			return nil, internal(err)
		}
	}
	if r.Outcome == "blocked" {
		if _, err = tx.Exec(ctx, store.Q("blocked"), chat, 1); err != nil {
			return nil, internal(err)
		}
		if _, err = tx.Exec(ctx, store.Q("block_pending"), chat); err != nil {
			return nil, internal(err)
		}
	}
	if r.Outcome == "permanent" && broadcast != nil {
		if _, err = tx.Exec(ctx, store.Q("pause_running"), *broadcast); err != nil {
			return nil, internal(err)
		}
	}
	if kind == "sync" && r.Outcome == "sent" {
		view := &pb.View{}
		if err = protojson.Unmarshal(body, view); err != nil {
			return nil, internal(err)
		}
		if r.NextAfter > 0 {
			view.Numbers = []int64{r.NextAfter, r.Checked, r.FailedMembers}
			err = enqueue(ctx, tx, chat, false, "sync", view, actor, 0, 0)
		} else {
			err = enqueue(ctx, tx, chat, false, "view", &pb.View{Kind: "sync_done", Numbers: []int64{r.Checked, r.FailedMembers}}, actor, 0, 0)
		}
		if err != nil {
			return nil, internal(err)
		}
	}
	if kind == "sync" && state == "failed" {
		if err = enqueue(ctx, tx, chat, false, "view", &pb.View{Kind: "notice", Code: "sync_denied"}, actor, 0, 0); err != nil {
			return nil, internal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &pb.Receipt{}, nil
}

func (s *Service) Export(ctx context.Context, r *pb.ExportRequest) (*pb.ExportResponse, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	if !s.allowed(ctx, tx, r.Actor, "table_viewer") {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	rows, err := tx.Query(ctx, store.Q("export"))
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	file := excelize.NewFile()
	defer file.Close()
	headers := []string{"telegram_id", "state", "username", "telegram_sername", "name", "birth_date", "group", "phone", "expectations", "will_drive", "trip_attendance", "is_staff", "is_counselor", "is_blocked", "first_start_source", "first_start_status", "first_start_at_utc"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err = file.SetCellStr("Sheet1", cell, h); err != nil {
			return nil, internal(err)
		}
	}
	n := 2
	for rows.Next() {
		if n > 50001 {
			return nil, status.Error(codes.ResourceExhausted, "export row limit")
		}
		values, err := rows.Values()
		if err != nil {
			return nil, internal(err)
		}
		for i, v := range values {
			text := ""
			switch value := v.(type) {
			case string:
				text = value
			case int64:
				text = strconv.FormatInt(value, 10)
			case int32:
				text = strconv.FormatInt(int64(value), 10)
			}
			cell, _ := excelize.CoordinatesToCellName(i+1, n)
			// SetCellStr, rather than formula-aware inference, prevents spreadsheet formula injection.
			if err = file.SetCellStr("Sheet1", cell, text); err != nil {
				return nil, internal(err)
			}
		}
		n++
	}
	if err = rows.Err(); err != nil {
		return nil, internal(err)
	}
	if _, err = tx.Exec(ctx, store.Q("audit"), r.Actor, "export", 0); err != nil {
		return nil, internal(err)
	}
	buffer, err := file.WriteToBuffer()
	if err != nil {
		return nil, internal(err)
	}
	if buffer.Len() > 12<<20 {
		return nil, status.Error(codes.ResourceExhausted, "export byte limit")
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &pb.ExportResponse{Xlsx: buffer.Bytes()}, nil
}
