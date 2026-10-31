package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

// The bounded questionnaire and previews fit Telegram text limits even with
// maximum-length valid fields. Closed questions count towards this event limit.
const maxPublishedQuestions = 10

type surveyOption struct {
	id    int64
	label string
}
type surveyQuestion struct {
	id, actor, broadcast int64
	title, body, status  string
	photo                string
	registration         bool
	options              []surveyOption
	answer               int64
}

func number(id int64) string      { return strconv.FormatInt(id, 10) }
func surveyState(id int64) string { return "survey_" + number(id) }
func loadSurveys(ctx context.Context, tx pgx.Tx, actor int64) ([]surveyQuestion, error) {
	return readSurveys(ctx, tx, actor, false)
}
func readSurveys(ctx context.Context, tx pgx.Tx, actor int64, allDrafts bool) ([]surveyQuestion, error) {
	rows, err := tx.Query(ctx, store.Q("survey_load"), actor, allDrafts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []surveyQuestion
	for rows.Next() {
		var q surveyQuestion
		var o surveyOption
		if err = rows.Scan(&q.id, &q.actor, &q.title, &q.body, &q.photo, &q.registration, &q.status, &q.broadcast, &o.id, &o.label, &q.answer); err != nil {
			return nil, err
		}
		if len(result) == 0 || result[len(result)-1].id != q.id {
			result = append(result, q)
		}
		result[len(result)-1].options = append(result[len(result)-1].options, o)
	}
	return result, rows.Err()
}
func findSurvey(questions []surveyQuestion, id int64) (surveyQuestion, bool) {
	for _, q := range questions {
		if q.id == id {
			return q, true
		}
	}
	return surveyQuestion{}, false
}
func (s *Service) nextUser(u user) string {
	if next := s.next(u.values); next != "confirm" {
		return next
	}
	for _, q := range u.surveys {
		if q.status == "open" && q.registration && q.answer == 0 {
			return surveyState(q.id)
		}
	}
	return "confirm"
}
func surveyView(q surveyQuestion, kind, callbackPrefix string) *pb.View {
	v := &pb.View{Kind: kind, Title: q.title, Text: q.body, Numbers: []int64{q.id}}
	if kind == "survey_question" || kind == "survey_preview" || kind == "survey_broadcast" {
		v.PhotoFileId = q.photo
	}
	if q.status == "open" || q.status == "draft" {
		for _, o := range q.options {
			v.Buttons = append(v.Buttons, &pb.Button{Label: o.label, Data: callbackPrefix + number(q.id) + "_" + number(o.id)})
		}
	}
	return v
}
func surveyInput(text string, limit int) bool {
	return strings.TrimSpace(text) != "" && len(utf16.Encode([]rune(text))) <= limit
}
func surveyOptions(text string) ([]string, bool) {
	options := strings.Split(strings.TrimSpace(text), "\n")
	if len(options) < 2 || len(options) > 10 {
		return nil, false
	}
	seen := map[string]bool{}
	for i, label := range options {
		label = strings.TrimSpace(label)
		if !surveyInput(label, 60) || seen[label] {
			return nil, false
		}
		options[i] = label
		seen[label] = true
	}
	return options, true
}

func parseSurveyAnswer(data, prefix string) (int64, int64, bool) {
	parts := strings.Split(strings.TrimPrefix(data, prefix), "_")
	if !strings.HasPrefix(data, prefix) || len(parts) != 2 {
		return 0, 0, false
	}
	q, e1 := strconv.ParseInt(parts[0], 10, 64)
	o, e2 := strconv.ParseInt(parts[1], 10, 64)
	return q, o, e1 == nil && e2 == nil && q > 0 && o > 0
}

// surveyCallback handles broadcasts independently of the current questionnaire.
// A second broadcast click only removes obsolete buttons; it never edits an answer.
func (s *Service) surveyCallback(ctx context.Context, tx pgx.Tx, in *pb.Update, u *user, r *replies) (bool, error) {
	if !strings.HasPrefix(in.Callback, "qa_") {
		return false, nil
	}
	stale := func() (bool, error) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "notice", Code: "stale"}, in.Actor, 0, 0)
	}
	id, option, ok := parseSurveyAnswer(in.Callback, "qa_")
	if !ok {
		return stale()
	}
	q, ok := findSurvey(u.surveys, id)
	if !ok || q.status == "draft" {
		return stale()
	}
	var targeted bool
	if err := tx.QueryRow(ctx, store.Q("survey_target"), q.broadcast, in.Actor).Scan(&targeted); err != nil {
		return true, err
	}
	if !targeted {
		return stale()
	}
	valid := false
	for _, o := range q.options {
		if o.id == option {
			valid = true
		}
	}
	if !valid {
		return stale()
	}
	if err := r.removeKeyboard(ctx, tx, in.Chat, false, in.Actor); err != nil {
		return true, err
	}
	if q.status != "open" || q.answer != 0 {
		return true, nil
	}
	tag, err := tx.Exec(ctx, store.Q("survey_answer"), q.id, in.Actor, option)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() == 0 {
		return true, nil
	}
	u.surveys, err = loadSurveys(ctx, tx, in.Actor)
	if err != nil {
		return true, err
	}
	// Show the existing questionnaire, without replacing the invitation or
	// exposing the recorded survey answer. Do not interrupt an unfinished form.
	if u.state == surveyState(q.id) {
		u.state = s.nextUser(*u)
		if err = saveUser(ctx, tx, in.Actor, u); err != nil {
			return true, err
		}
	}
	sends := *r
	sends.sendOnly = true
	return true, sends.enqueue(ctx, tx, in.Chat, false, "view", s.view(*u), in.Actor, 0, 0)
}

func (s *Service) surveyFormAnswer(ctx context.Context, tx pgx.Tx, in *pb.Update, u *user, action string) (bool, error) {
	if !strings.HasPrefix(u.state, "survey_") {
		return false, nil
	}
	field := u.state
	id, err := strconv.ParseInt(strings.TrimPrefix(field, "survey_"), 10, 64)
	q, ok := findSurvey(u.surveys, id)
	if err != nil || !ok || q.status != "open" || !q.registration {
		u.state = s.nextUser(*u)
		return true, nil
	}
	if q.answer != 0 {
		u.state = s.nextUser(*u)
		return true, nil
	}
	qid, option, valid := parseSurveyAnswer(action, "sq_")
	if !valid || qid != id {
		return true, nil
	}
	valid = false
	for _, o := range q.options {
		if o.id == option {
			valid = true
		}
	}
	if !valid {
		return true, nil
	}
	if _, err = tx.Exec(ctx, store.Q("survey_answer"), id, in.Actor, option); err != nil {
		return true, err
	}
	u.surveys, err = loadSurveys(ctx, tx, in.Actor)
	if err != nil {
		return true, err
	}
	u.state = s.nextUser(*u)
	return true, nil
}

func surveyLifecycleCommand(text string) bool {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return false
	}
	cmd := strings.Split(parts[0], "@")[0]
	return cmd == "/question_publish" || cmd == "/question_close"
}

// Refresh queued invitations and receipts at claim time: an already answered or
// closed question must not regain its keyboard after a retry or slow broadcast.
func refreshSurveyDelivery(ctx context.Context, tx pgx.Tx, d *pb.Delivery) (bool, error) {
	if d.Kind != "view" || d.View == nil {
		return true, nil
	}
	switch d.View.Kind {
	case "survey_broadcast", "survey_question":
	default:
		return true, nil
	}
	if len(d.View.Numbers) != 1 {
		return false, fmt.Errorf("invalid survey delivery")
	}
	qs, err := loadSurveys(ctx, tx, d.Chat)
	if err != nil {
		return false, err
	}
	q, ok := findSurvey(qs, d.View.Numbers[0])
	if !ok {
		return false, fmt.Errorf("missing survey question")
	}
	if q.status != "open" || q.answer != 0 {
		return false, nil
	}
	if d.View.Kind == "survey_broadcast" {
		d.View = surveyView(q, "survey_broadcast", "qa_")
	}
	return true, nil
}

type surveyExportColumn struct {
	id    int64
	title string
}

func loadSurveyExport(ctx context.Context, tx pgx.Tx) ([]surveyExportColumn, map[int64]map[int64]string, error) {
	rows, err := tx.Query(ctx, store.Q("survey_export_questions"))
	if err != nil {
		return nil, nil, err
	}
	var columns []surveyExportColumn
	for rows.Next() {
		var q surveyExportColumn
		if err = rows.Scan(&q.id, &q.title); err != nil {
			rows.Close()
			return nil, nil, err
		}
		columns = append(columns, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	answers := map[int64]map[int64]string{}
	rows, err = tx.Query(ctx, store.Q("survey_export_answers"))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actor, qid int64
		var answer string
		if err = rows.Scan(&actor, &qid, &answer); err != nil {
			return nil, nil, err
		}
		if answers[actor] == nil {
			answers[actor] = map[int64]string{}
		}
		answers[actor][qid] = answer
	}
	return columns, answers, rows.Err()
}
