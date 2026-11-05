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

type finalCampaign struct {
	question, positive, draftID, sourceChat, sourceMessage int64
	positiveLabel, negativeLabel                           string
}

type finalDraft struct {
	id, question, positive       int64
	sourceChat, sourceMessage    int64
	previewChat, previewMessage  int64
	positiveLabel, negativeLabel string
}

func (d finalDraft) hasLabels() bool { return d.positiveLabel != "" && d.negativeLabel != "" }

// The first label always confirms participation; the second always declines.
// Reuse question-option normalization and UTF-16 limits without changing the
// stable callback values or stored outcome strings.
func finalButtonLabels(text string) (positive, negative string, ok bool) {
	options, valid := surveyOptions(text)
	if !valid || len(options) != 2 || strings.EqualFold(options[0], options[1]) {
		return "", "", false
	}
	return options[0], options[1], true
}

func readFinalCampaign(ctx context.Context, tx pgx.Tx) (finalCampaign, error) {
	var c finalCampaign
	err := tx.QueryRow(ctx, store.Q("final_campaign")).Scan(&c.question, &c.positive, &c.draftID, &c.sourceChat, &c.sourceMessage, &c.positiveLabel, &c.negativeLabel)
	return c, err
}

func readFinalDraft(ctx context.Context, tx pgx.Tx, actor int64) (finalDraft, error) {
	var d finalDraft
	err := tx.QueryRow(ctx, store.Q("final_draft_get"), actor).Scan(&d.id, &d.question, &d.positive, &d.sourceChat, &d.sourceMessage, &d.previewChat, &d.previewMessage, &d.positiveLabel, &d.negativeLabel)
	return d, err
}

// Preparing a final confirmation owns the next message only. The saved draft
// and its preview survive restarts independently of the participant's form.
func (s *Service) finalDraftMessage(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies) (bool, error) {
	if strings.HasPrefix(in.Callback, "preview_final_") {
		return true, nil
	}
	d, err := readFinalDraft(ctx, tx, in.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if in.Text == "/cancel" || in.Text == "/start" || !s.allowed(ctx, tx, in.Actor, "message_sender") {
		_, err = tx.Exec(ctx, store.Q("final_draft_delete"), in.Actor)
		return false, err
	}
	if d.sourceMessage != 0 && d.hasLabels() {
		return false, nil
	}
	if d.sourceMessage == 0 {
		code := "final_send_source"
		if in.Callback == "" && in.MessageId > 0 {
			if _, err = tx.Exec(ctx, store.Q("final_draft_capture"), in.Actor, in.Chat, in.MessageId); err != nil {
				return true, err
			}
			code = "final_buttons_prompt"
		}
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_manage", Code: code}, in.Actor, 0, 0)
	}
	positive, negative, valid := finalButtonLabels(in.Text)
	if in.Callback != "" || in.PhotoFileId != "" || !valid {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_manage", Code: "final_buttons_prompt"}, in.Actor, 0, 0)
	}
	if _, err = tx.Exec(ctx, store.Q("final_draft_labels"), in.Actor, positive, negative); err != nil {
		return true, err
	}
	d, err = readFinalDraft(ctx, tx, in.Actor)
	if err != nil {
		return true, err
	}
	return true, s.finalPreview(ctx, tx, in, r, d)
}

func finalPreviewView(d finalDraft) *pb.View {
	v := finalInvitation(d.positiveLabel, d.negativeLabel)
	v.Kind, v.Numbers = "final_preview", []int64{d.id}
	for i, button := range v.Buttons {
		button.Data = "preview_final_" + number(int64(i))
	}
	return v
}

func (s *Service) finalPreview(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, d finalDraft) error {
	sourceChat, sourceMessage := d.sourceChat, d.sourceMessage
	if d.previewMessage > 0 {
		sourceChat, sourceMessage = d.previewChat, d.previewMessage
	}
	v := finalPreviewView(d)
	if err := r.enqueue(ctx, tx, in.Chat, false, "copy", v, in.Actor, sourceChat, sourceMessage); err != nil {
		return err
	}
	info, err := finalSummary(ctx, tx, d.question, d.positive, "final_preview_info")
	if err != nil {
		return err
	}
	info.Fields = append(info.Fields,
		&pb.Field{Key: "final_positive_button", Value: d.positiveLabel},
		&pb.Field{Key: "final_negative_button", Value: d.negativeLabel},
		&pb.Field{Key: "final_draft", Value: number(d.id)},
		&pb.Field{Key: "final_start_command", Value: "/final_start " + number(d.id)},
	)
	return r.enqueue(ctx, tx, in.Chat, false, "view", info, in.Actor, 0, 0)
}

func (s *Service) finalAdmin(ctx context.Context, tx pgx.Tx, in *pb.Update, u *user, r *replies, parts []string) (bool, error) {
	cmd := strings.Split(parts[0], "@")[0]
	permission, kind := "message_sender", "final_manage"
	if cmd == "/final_status" {
		permission, kind = "table_viewer", "final_status"
	}
	if in.ChatType != "private" || !s.allowed(ctx, tx, in.Actor, permission) {
		return true, nil
	}
	reply := func(v *pb.View) (bool, error) {
		v.Kind = kind
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0)
	}
	notice := func(code string) (bool, error) { return reply(&pb.View{Code: code}) }
	if cmd == "/final_new" && len(parts) != 3 ||
		(cmd == "/final_preview" || cmd == "/final_start") && len(parts) != 2 ||
		(cmd == "/final_status" || cmd == "/final_retry") && len(parts) != 1 {
		return notice("final_usage")
	}
	if _, err := tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	c, err := readFinalCampaign(ctx, tx)
	started := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return true, err
	}
	if cmd == "/final_new" {
		if started {
			return notice("final_already_started")
		}
		q, p, ok := parseSurveyAnswer("qa_"+parts[1]+"_"+parts[2], "qa_")
		if !ok {
			return notice("final_usage")
		}
		var title, label string
		err = tx.QueryRow(ctx, store.Q("final_source"), q, p).Scan(&title, &label)
		if errors.Is(err, pgx.ErrNoRows) {
			return notice("final_invalid_source")
		}
		if err != nil {
			return true, err
		}
		if strings.HasPrefix(u.state, "broadcast_") {
			u.state = s.nextUser(*u)
			if err = saveUser(ctx, tx, in.Actor, u); err != nil {
				return true, err
			}
		}
		if _, err = tx.Exec(ctx, store.Q("survey_draft_delete"), in.Actor); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("final_draft_new"), in.Actor, q, p); err != nil {
			return true, err
		}
		return notice("final_send_source")
	}
	if cmd == "/final_preview" || cmd == "/final_start" {
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return notice("final_usage")
		}
		if started && cmd == "/final_start" {
			if c.draftID != id {
				return notice("final_already_started")
			}
		} else {
			d, err := readFinalDraft(ctx, tx, in.Actor)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && (d.id != id || d.sourceMessage == 0) {
				return notice("final_draft_not_found")
			}
			if err != nil {
				return true, err
			}
			if !d.hasLabels() {
				return notice("final_buttons_prompt")
			}
			if cmd == "/final_preview" {
				return true, s.finalPreview(ctx, tx, in, r, d)
			}
			if d.previewMessage <= 0 {
				return notice("final_preview_pending")
			}
			var count, missing int64
			if err = tx.QueryRow(ctx, store.Q("final_eligible_count"), d.question, d.positive).Scan(&count, &missing); err != nil {
				return true, err
			}
			if missing != 0 {
				return notice("final_missing_order")
			}
			if _, err = tx.Exec(ctx, store.Q("final_create"), d.question, d.positive, in.Actor, d.id, d.previewChat, d.previewMessage, d.positiveLabel, d.negativeLabel); err != nil {
				return true, err
			}
			if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "final_start", d.question); err != nil {
				return true, err
			}
			c, started = finalCampaign{question: d.question, positive: d.positive}, true
		}
	}
	if !started {
		return notice("final_not_started")
	}
	if cmd == "/final_start" || cmd == "/final_retry" {
		if err = fillFinalConfirmations(ctx, tx); err != nil {
			return true, err
		}
	}
	if cmd == "/final_retry" {
		if _, err = tx.Exec(ctx, store.Q("final_retry")); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "final_retry", c.question); err != nil {
			return true, err
		}
	}
	v, err := finalSummary(ctx, tx, c.question, c.positive, "final_status_info")
	if err != nil {
		return true, err
	}
	return reply(v)
}

func finalSummary(ctx context.Context, tx pgx.Tx, question, positive int64, code string) (*pb.View, error) {
	var title, label string
	if err := tx.QueryRow(ctx, store.Q("final_source"), question, positive).Scan(&title, &label); err != nil {
		return nil, err
	}
	var count, missing int64
	if err := tx.QueryRow(ctx, store.Q("final_eligible_count"), question, positive).Scan(&count, &missing); err != nil {
		return nil, err
	}
	v := &pb.View{Kind: "final_manage", Code: code, Fields: []*pb.Field{
		{Key: "final_source_question", Value: number(question) + " · " + title},
		{Key: "final_source_option", Value: number(positive) + " · " + label},
		{Key: "final_capacity", Value: number(finalCapacity)},
		{Key: "final_eligible", Value: number(count)},
		{Key: "final_missing_time", Value: number(missing)},
	}}
	counts := map[string]int64{}
	rows, err := tx.Query(ctx, store.Q("final_counts"))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var n int64
		if err = rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		counts[state] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, state := range []string{"invited", "yes", "no", "excluded", "late"} {
		v.Fields = append(v.Fields, &pb.Field{Key: "final_count_" + state, Value: number(counts[state])})
	}
	var failed int64
	if err = tx.QueryRow(ctx, store.Q("final_failures")).Scan(&failed); err != nil {
		return nil, err
	}
	v.Fields = append(v.Fields, &pb.Field{Key: "final_failed", Value: number(failed)})
	return v, nil
}
