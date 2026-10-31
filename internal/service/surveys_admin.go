package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

// surveyDraft is independent of the participant's questionnaire state.
func (s *Service) surveyDraft(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies) (bool, error) {
	var draftID int64
	var stage, title, body, photo string
	var options []string
	err := tx.QueryRow(ctx, store.Q("survey_draft_get"), in.Actor).Scan(&draftID, &stage, &title, &body, &options, &photo)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if in.Text == "/cancel" || in.Text == "/start" || !s.allowed(ctx, tx, in.Actor, "message_sender") {
		_, err = tx.Exec(ctx, store.Q("survey_draft_delete"), in.Actor)
		return false, err
	}
	reply := func(code string) (bool, error) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: code}, in.Actor, 0, 0)
	}
	if in.Callback != "" && !strings.HasPrefix(in.Callback, "qm:") {
		return reply("survey_draft_busy")
	}
	if in.PhotoFileId != "" && stage == "title" {
		body = strings.TrimSpace(in.PhotoCaption)
		if body != "" && !surveyInput(body, 1024) {
			return reply("survey_photo_caption_prompt")
		}
		photo = in.PhotoFileId
		if _, err = tx.Exec(ctx, store.Q("survey_draft_save"), in.Actor, stage, title, body, options, photo); err != nil {
			return true, err
		}
		return reply("survey_photo_title_prompt")
	}
	if in.PhotoFileId != "" && stage != "body" {
		if stage == "mode" {
			return s.surveyMode(ctx, tx, in, r, draftID)
		}
		return reply("survey_" + stage + "_prompt")
	}
	switch stage {
	case "title":
		if in.Callback != "" || !surveyInput(in.Text, 80) || strings.Contains(in.Text, "\n") {
			return reply("survey_title_prompt")
		}
		title = strings.TrimSpace(in.Text)
		if photo != "" {
			if body == "" {
				body = title
			}
			stage = "options"
		} else {
			stage = "body"
		}
	case "body":
		if in.PhotoFileId != "" {
			body = strings.TrimSpace(in.PhotoCaption)
			if body == "" {
				body = title
			}
			if !surveyInput(body, 1024) {
				return reply("survey_photo_caption_prompt")
			}
			photo = in.PhotoFileId
		} else {
			if in.Callback != "" || !surveyInput(in.Text, 1800) {
				return reply("survey_body_prompt")
			}
			body = strings.TrimSpace(in.Text)
			photo = ""
		}
		stage = "options"
	case "options":
		var ok bool
		options, ok = surveyOptions(in.Text)
		if in.Callback != "" || !ok {
			return reply("survey_options_prompt")
		}
		stage = "mode"
	case "mode":
		if in.Callback != fmt.Sprintf("qm:%d:0", draftID) && in.Callback != fmt.Sprintf("qm:%d:1", draftID) {
			return s.surveyMode(ctx, tx, in, r, draftID)
		}
		var id int64
		if err = tx.QueryRow(ctx, store.Q("survey_create"), in.Actor, title, body, in.Callback == fmt.Sprintf("qm:%d:1", draftID), photo).Scan(&id); err != nil {
			return true, err
		}
		for i, label := range options {
			if _, err = tx.Exec(ctx, store.Q("survey_option_create"), id, i, label); err != nil {
				return true, err
			}
		}
		if _, err = tx.Exec(ctx, store.Q("survey_draft_delete"), in.Actor); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "question_create", id); err != nil {
			return true, err
		}
		return true, s.surveyPreview(ctx, tx, in, r, id)
	}
	if _, err = tx.Exec(ctx, store.Q("survey_draft_save"), in.Actor, stage, title, body, options, photo); err != nil {
		return true, err
	}
	if stage == "mode" {
		return s.surveyMode(ctx, tx, in, r, draftID)
	}
	return reply("survey_" + stage + "_prompt")
}
func (s *Service) surveyMode(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, draftID int64) (bool, error) {
	return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: "survey_mode_prompt", Buttons: []*pb.Button{
		{LabelKey: "survey_mode_yes", Data: fmt.Sprintf("qm:%d:1", draftID)}, {LabelKey: "survey_mode_no", Data: fmt.Sprintf("qm:%d:0", draftID)},
	}}, in.Actor, 0, 0)
}
func (s *Service) surveyPreview(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, id int64) error {
	qs, err := readSurveys(ctx, tx, in.Actor, s.allowed(ctx, tx, in.Actor, "admin"))
	if err != nil {
		return err
	}
	q, ok := findSurvey(qs, id)
	if !ok {
		return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: "survey_not_found"}, in.Actor, 0, 0)
	}
	preview := q
	preview.status = "draft"
	v := surveyView(preview, "survey_preview", "preview_")
	if err = r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0); err != nil {
		return err
	}
	var count int64
	if err = tx.QueryRow(ctx, store.Q("survey_audience_count")).Scan(&count); err != nil {
		return err
	}
	if q.status != "draft" {
		return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: "survey_existing_" + q.status, Numbers: []int64{id, q.broadcast}}, in.Actor, 0, 0)
	}
	code := "survey_preview_info"
	if q.registration {
		code = "survey_preview_registration"
	}
	return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: code, Numbers: []int64{id, count}}, in.Actor, 0, 0)
}
func (s *Service) surveyAdmin(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, parts []string) (bool, error) {
	cmd := strings.Split(parts[0], "@")[0]
	read := cmd == "/questions" || cmd == "/question_results"
	if in.ChatType != "private" || (!read && !s.allowed(ctx, tx, in.Actor, "message_sender")) || (read && !s.allowed(ctx, tx, in.Actor, "table_viewer") && !(cmd == "/questions" && s.allowed(ctx, tx, in.Actor, "message_sender"))) {
		return true, nil
	}
	reply := func(code string) (bool, error) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_notice", Code: code}, in.Actor, 0, 0)
	}
	if cmd == "/question_new" {
		if len(parts) != 1 {
			return reply("survey_usage")
		}
		_, err := tx.Exec(ctx, store.Q("survey_draft_start"), in.Actor)
		if err != nil {
			return true, err
		}
		return reply("survey_title_prompt")
	}
	qs, err := readSurveys(ctx, tx, in.Actor, s.allowed(ctx, tx, in.Actor, "admin"))
	if err != nil {
		return true, err
	}
	if cmd == "/questions" {
		page, ok := surveyPage(parts, 1)
		if !ok {
			return reply("survey_usage")
		}
		start := (page - 1) * 10
		v := &pb.View{Kind: "survey_list", Numbers: []int64{page}}
		for i := start; i < int64(len(qs)) && i < start+10; i++ {
			q := qs[i]
			v.Fields = append(v.Fields, &pb.Field{Key: number(q.id) + " · " + q.title, Value: q.status})
		}
		if start+10 < int64(len(qs)) {
			v.Numbers = append(v.Numbers, page+1)
		}
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0)
	}
	if len(parts) < 2 || len(parts) > 3 || (cmd != "/question_results" && len(parts) != 2) {
		return reply("survey_usage")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return reply("survey_usage")
	}
	q, ok := findSurvey(qs, id)
	if !ok {
		return reply("survey_not_found")
	}
	if read {
		return true, s.surveyResults(ctx, tx, in, r, q, parts)
	}
	if q.actor != in.Actor && !s.allowed(ctx, tx, in.Actor, "admin") {
		return true, nil
	}
	switch cmd {
	case "/question_preview":
		return true, s.surveyPreview(ctx, tx, in, r, id)
	case "/question_publish":
		if q.status != "draft" {
			return reply("survey_invalid_status")
		}
		var n int
		if err = tx.QueryRow(ctx, store.Q("survey_published_count")).Scan(&n); err != nil {
			return true, err
		}
		if n >= maxPublishedQuestions {
			return reply("survey_limit")
		}
		var broadcast int64
		if err = tx.QueryRow(ctx, store.Q("broadcast_create"), in.Actor, "registered", "survey", int64(0), int64(0)).Scan(&broadcast); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("survey_publish"), id, broadcast); err != nil {
			return true, err
		}
		q.status = "open"
		data, err := protojson.Marshal(surveyView(q, "survey_broadcast", "qa_"))
		if err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("survey_broadcast_start"), broadcast, data, in.Actor, carrier(ctx)); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("broadcast_status"), broadcast, "running"); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "question_publish", id); err != nil {
			return true, err
		}
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_manage", Code: "survey_published", Numbers: []int64{id, broadcast}}, in.Actor, 0, 0)
	case "/question_close":
		if q.status != "open" {
			return reply("survey_invalid_status")
		}
		if _, err = tx.Exec(ctx, store.Q("survey_close"), id); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("broadcast_cancel"), q.broadcast); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("broadcast_status"), q.broadcast, "cancelled"); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "question_close", id); err != nil {
			return true, err
		}
		return reply("survey_closed")
	}
	return reply("survey_usage")
}
func surveyPage(parts []string, index int) (int64, bool) {
	if len(parts) == index {
		return 1, true
	}
	if len(parts) != index+1 {
		return 0, false
	}
	page, err := strconv.ParseInt(parts[index], 10, 32)
	return page, err == nil && page > 0
}
func (s *Service) surveyResults(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, q surveyQuestion, parts []string) error {
	page, ok := surveyPage(parts, 2)
	if !ok {
		return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "survey_notice", Code: "survey_usage"}, in.Actor, 0, 0)
	}
	v := &pb.View{Kind: "survey_results", Title: q.title, Numbers: []int64{q.id, page}}
	rows, err := tx.Query(ctx, store.Q("survey_counts"), q.id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var label string
		var count int64
		if err = rows.Scan(&label, &count); err != nil {
			rows.Close()
			return err
		}
		v.Fields = append(v.Fields, &pb.Field{Key: label, Value: number(count)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var unanswered int64
	if err = tx.QueryRow(ctx, store.Q("survey_unanswered"), q.id).Scan(&unanswered); err != nil {
		return err
	}
	v.Code = number(unanswered)
	if err = r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0); err != nil {
		return err
	}
	people := &pb.View{Kind: "survey_people", Title: q.title, Numbers: []int64{q.id, page}}
	rows, err = tx.Query(ctx, store.Q("survey_results"), q.id, (page-1)*10)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var actor int64
		var name, answer string
		if err = rows.Scan(&actor, &name, &answer); err != nil {
			return err
		}
		if len(people.Fields) == 10 {
			people.Numbers = append(people.Numbers, page+1)
			break
		}
		// Legacy names may exceed current validation limits.
		name = string([]rune(name)[:min(len([]rune(name)), 150)])
		people.Fields = append(people.Fields, &pb.Field{Key: number(actor) + " · " + name, Value: answer})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	return r.enqueue(ctx, tx, in.Chat, false, "view", people, in.Actor, 0, 0)
}
