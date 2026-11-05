package service

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func (s *Service) finalDeclineAllowed(ctx context.Context, tx pgx.Tx, actor int64) bool {
	return s.allowed(ctx, tx, actor, "admin") && s.allowed(ctx, tx, actor, "message_sender")
}

func (s *Service) finalDeclinePreview(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, parts []string) (bool, error) {
	if in.ChatType != "private" || !s.finalDeclineAllowed(ctx, tx, in.Actor) {
		return true, nil
	}
	reply := func(code string) (bool, error) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_decline_result", Code: code}, in.Actor, 0, 0)
	}
	if len(parts) != 2 {
		return reply("final_decline_usage")
	}
	target, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		username := strings.TrimPrefix(parts[1], "@")
		if len(username) == 0 || len(username) > 32 || strings.IndexFunc(username, func(r rune) bool {
			return r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
		}) >= 0 {
			return reply("final_decline_id_required")
		}
		rows, err := tx.Query(ctx, store.Q("final_decline_username"), username)
		if err != nil {
			return true, err
		}
		matches, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return true, err
		}
		if len(matches) != 1 {
			return reply("final_decline_id_required")
		}
		target = matches[0]
	}
	if target <= 0 {
		return reply("final_decline_usage")
	}
	if _, err = tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	var state, name string
	var message int64
	err = tx.QueryRow(ctx, store.Q("final_decline_target"), target).Scan(&state, &name, &message)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state == "excluded" {
		return reply("final_decline_not_active")
	}
	if err != nil {
		return true, err
	}
	if state == "no" {
		return reply("final_decline_already_no")
	}
	v := &pb.View{Kind: "final_decline_preview", Code: "final_decline_confirm", Fields: []*pb.Field{
		{Key: "final_decline_person", Value: name},
		{Key: "final_decline_id", Value: number(target)},
		{Key: "final_count_" + state, Value: "1"},
	}, Buttons: []*pb.Button{
		{LabelKey: "final_decline_button", Data: "final_decline:" + number(target) + ":" + state},
		{LabelKey: "cancel", Data: "final_decline_cancel"},
	}}
	return true, r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0)
}

// Only a successfully delivered preview in this operator's private chat can
// authorize the override. The expected state prevents an old preview from
// overriding a newer participant answer without reviewing it again.
func (s *Service) finalDeclineCallback(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies) (bool, error) {
	if !strings.HasPrefix(in.Callback, "final_decline:") && in.Callback != "final_decline_cancel" {
		return false, nil
	}
	if in.ChatType != "private" || !s.finalDeclineAllowed(ctx, tx, in.Actor) {
		return true, nil
	}
	reply := func(code string) (bool, error) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_decline_result", Code: code}, in.Actor, 0, 0)
	}
	preview, err := tx.Exec(ctx, store.Q("final_decline_consume_preview"), in.Actor, in.MessageId, in.Callback)
	if err != nil {
		return true, err
	}
	if preview.RowsAffected() == 0 {
		return reply("final_decline_stale")
	}
	if err := r.removeKeyboard(ctx, tx, in.Chat, false, in.Actor); err != nil {
		return true, err
	}
	if in.Callback == "final_decline_cancel" {
		return reply("final_decline_cancelled")
	}
	parts := strings.Split(in.Callback, ":")
	if len(parts) != 3 || (parts[2] != "yes" && parts[2] != "invited") {
		return reply("final_decline_stale")
	}
	target, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || target <= 0 {
		return reply("final_decline_stale")
	}
	if _, err = tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	var state, name string
	var message int64
	err = tx.QueryRow(ctx, store.Q("final_decline_target"), target).Scan(&state, &name, &message)
	if errors.Is(err, pgx.ErrNoRows) {
		return reply("final_decline_not_active")
	}
	if err != nil {
		return true, err
	}
	if state == "no" {
		return reply("final_decline_already_no")
	}
	if state != parts[2] {
		return reply("final_decline_stale")
	}
	tag, err := tx.Exec(ctx, store.Q("final_admin_decline"), target, state)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() == 0 {
		return reply("final_decline_stale")
	}
	if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "final_admin_decline_"+state, target); err != nil {
		return true, err
	}
	if message > 0 {
		if err = r.enqueue(ctx, tx, target, false, "survey_remove_keyboard", nil, target, 0, message); err != nil {
			return true, err
		}
	}
	if err = fillFinalConfirmations(ctx, tx); err != nil {
		return true, err
	}
	return reply("final_decline_done")
}
