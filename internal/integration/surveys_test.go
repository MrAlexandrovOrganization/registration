//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

type surveyFixture struct {
	t   *testing.T
	ctx context.Context
	db  *pgxpool.Pool
	svc *service.Service
	seq atomic.Int64
}

func newSurveyFixture(t *testing.T, schema string) *surveyFixture {
	t.Helper()
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || cfg.ConnConfig.Database != "registration_test" {
		t.Fatal("requires disposable registration_test")
	}
	bootstrap, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bootstrap.Close()
	if _, err = bootstrap.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &surveyFixture{t: t, ctx: ctx, db: db, svc: &service.Service{DB: db, BotID: 1000, RootID: 1}}
	return f
}
func (f *surveyFixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *surveyFixture) exec(sql string, args ...any) {
	f.t.Helper()
	_, err := f.db.Exec(f.ctx, sql, args...)
	f.must(err)
}
func (f *surveyFixture) send(actor int64, text, callback string) (*pb.Receipt, *pb.View) {
	f.t.Helper()
	r, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: actor, Chat: actor, ChatType: "private", Text: text, Callback: callback, MessageId: 700, CallbackMessageEditable: callback != ""})
	f.must(err)
	v := &pb.View{}
	if len(r.DeliveryIds) > 0 {
		var body []byte
		f.must(f.db.QueryRow(f.ctx, "SELECT body FROM outbound_messages WHERE id=$1", r.DeliveryIds[len(r.DeliveryIds)-1]).Scan(&body))
		f.must(protojson.Unmarshal(body, v))
	}
	return r, v
}
func (f *surveyFixture) click(actor int64, v *pb.View, label string) *pb.View {
	f.t.Helper()
	for _, b := range v.Buttons {
		if b.LabelKey == label || b.Label == label {
			_, next := f.send(actor, "", b.Data)
			return next
		}
	}
	f.t.Fatalf("button %q missing in %v", label, v)
	return nil
}
func (f *surveyFixture) create(title string, registration bool) int64 {
	f.t.Helper()
	return f.createAudience(title, registration, "registered")
}
func (f *surveyFixture) createAudience(title string, registration bool, audience string) int64 {
	f.t.Helper()
	f.send(1, "/question_new", "")
	f.send(1, title, "")
	f.send(1, "<Текст> & вопрос?", "")
	_, prompt := f.send(1, "Да <&>\nНет", "")
	index := 1
	if registration {
		index = 0
	}
	_, prompt = f.send(1, "", prompt.Buttons[index].Data)
	var callback string
	for _, b := range prompt.Buttons {
		if b.LabelKey == "audience_"+audience {
			callback = b.Data
		}
	}
	if callback == "" {
		f.t.Fatal("missing audience", audience, prompt)
	}
	r, v := f.send(1, "", callback)
	if len(r.DeliveryIds) != 2 || len(v.Numbers) != 2 {
		f.t.Fatal("missing preview", v)
	}
	return v.Numbers[0]
}
func (f *surveyFixture) options(q int64) []int64 {
	f.t.Helper()
	rows, err := f.db.Query(f.ctx, "SELECT id FROM survey_options WHERE question_id=$1 ORDER BY position", q)
	f.must(err)
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		f.must(rows.Scan(&id))
		ids = append(ids, id)
	}
	f.must(rows.Err())
	return ids
}
func (f *surveyFixture) answer(q, actor int64) int64 {
	f.t.Helper()
	var id int64
	f.must(f.db.QueryRow(f.ctx, "SELECT COALESCE((SELECT option_id FROM survey_answers WHERE question_id=$1 AND telegram_id=$2),0)", q, actor).Scan(&id))
	return id
}
func (f *surveyFixture) seeded(actor int64, state string) {
	f.exec(`INSERT INTO users(telegram_id,state,name,birth_date,"group",phone,expectations,has_registered)
 VALUES($1,$2,'Synthetic','01.01.2000','ИУ7-41','79991234567','Synthetic',$2='registered')`, actor, state)
}
func (f *surveyFixture) claim(id int64) *pb.Delivery {
	f.t.Helper()
	d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: id, Worker: "survey-fixture-worker"})
	f.must(err)
	if d.Id != id {
		f.t.Fatal("job not claimed", id, d)
	}
	return d
}
func (f *surveyFixture) complete(d *pb.Delivery) {
	f.t.Helper()
	_, err := f.svc.Complete(f.ctx, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: "sent", TelegramMessageId: 700})
	f.must(err)
}

func TestSurveyQuestions(t *testing.T) {
	f := newSurveyFixture(t, "surveys_test")
	for _, actor := range []int64{42, 43, 44, 45} {
		f.seeded(actor, "registered")
	}
	f.exec("UPDATE users SET is_blocked=1 WHERE telegram_id=44")
	q := f.create("Трансфер <&>", true)
	options := f.options(q)
	f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
	f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM broadcasts").Scan(&count))
	if count != 1 {
		t.Fatal("duplicate publication")
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id IS NOT NULL").Scan(&count))
	if count != 3 {
		t.Fatal("wrong audience", count)
	}
	var broadcastJob int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id IS NOT NULL AND chat=42").Scan(&broadcastJob))
	d := f.claim(broadcastJob)
	if d.View.Kind != "survey_broadcast" || len(d.View.Buttons) != 2 {
		t.Fatal(d)
	}
	f.complete(d)
	r, v := f.send(42, "", fmt.Sprintf("qa_%d_%d", q, options[0]))
	if v.Kind != "registered" || v.Text != "" || v.Code != "" || len(v.Fields) != 5 || len(r.DeliveryIds) != 2 {
		t.Fatal("expected only saved questionnaire after answering", v, r)
	}
	replay, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Load(), Actor: 42, Chat: 42, ChatType: "private", Callback: fmt.Sprintf("qa_%d_%d", q, options[0])})
	f.must(err)
	if !replay.Duplicate || !slices.Equal(r.DeliveryIds, replay.DeliveryIds) {
		t.Fatal("lost callback receipt")
	}
	d = f.claim(r.DeliveryIds[0])
	if d.Kind != "survey_remove_keyboard" || d.SourceMessage != 700 {
		t.Fatal("original invitation buttons retained", d)
	}
	f.complete(d)
	d = f.claim(r.DeliveryIds[1])
	if d.View.Kind != "registered" || d.EditMessageId != 0 {
		t.Fatal("questionnaire replaced the invitation", d)
	}
	f.complete(d)
	r, _ = f.send(42, "", fmt.Sprintf("qa_%d_%d", q, options[1]))
	if f.answer(q, 42) != options[0] || len(r.DeliveryIds) != 1 {
		t.Fatal("second click changed answer or resent questionnaire")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, option := range options {
		wg.Go(func() {
			_, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 43, Chat: 43, ChatType: "private", Callback: fmt.Sprintf("qa_%d_%d", q, option), MessageId: 701, CallbackMessageEditable: true})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		f.must(err)
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM survey_answers WHERE question_id=$1 AND telegram_id=43", q).Scan(&count))
	if count != 1 {
		t.Fatal("duplicate answer")
	}
	f.send(50, "", fmt.Sprintf("qa_%d_%d", q, options[0]))
	if f.answer(q, 50) != 0 {
		t.Fatal("accepted untargeted callback")
	}
	_, v = f.send(42, "/start", "")
	if v.Kind != "registered" || len(v.Fields) != 5 {
		t.Fatal("survey answer exposed in questionnaire", v)
	}
	v = f.click(42, v, "edit")
	for _, button := range v.Buttons {
		if strings.Contains(button.Data, "survey_") {
			t.Fatal("survey offered for editing")
		}
	}
	var version int64
	f.must(f.db.QueryRow(f.ctx, "SELECT version FROM users WHERE telegram_id=42").Scan(&version))
	_, v = f.send(42, "", fmt.Sprintf("c:%d:edit_survey_%d", version, q))
	if v.Code != "stale" || f.answer(q, 42) != options[0] {
		t.Fatal("forged survey edit accepted", v)
	}
	_, v = f.send(42, "/cancel", "")
	f.click(42, v, "confirm")
	_, v = f.send(51, "/start", "")
	for _, text := range []string{"Test User", "1.1.2000", "ИУ7-41", "79991234567", "Synthetic"} {
		_, v = f.send(51, text, "")
	}
	if v.Kind != "survey_question" {
		t.Fatal("missing registration question", v)
	}
	_, invalid := f.send(51, "Да <&>", "")
	if f.answer(q, 51) != 0 || invalid.Kind != "survey_question" {
		t.Fatal("free text accepted")
	}
	_, v = f.send(51, "", invalid.Buttons[0].Data)
	if v.Kind != "confirm" || f.answer(q, 51) != options[0] || len(v.Fields) != 5 {
		t.Fatal("survey answer missing or exposed", v)
	}
	v = f.click(51, v, "confirm")
	if v.Kind != "registered" || len(v.Fields) != 5 {
		t.Fatal("registration failed")
	}
	f.exec("UPDATE users SET state='new' WHERE telegram_id=42")
	_, v = f.send(42, "/start", "")
	if v.Kind != "confirm" {
		t.Fatal("answered question asked twice", v)
	}
	f.click(42, v, "confirm")
	_, v = f.send(1, "/question_results "+strconv.FormatInt(q, 10), "")
	if v.Kind != "survey_people" || len(v.Fields) < 3 {
		t.Fatal("missing named results", v)
	}
	denied, _ := f.send(50, "/question_results "+strconv.FormatInt(q, 10), "")
	if len(denied.DeliveryIds) != 0 {
		t.Fatal("results exposed")
	}
	exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
	f.must(err)
	file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
	f.must(err)
	defer file.Close()
	rows, err := file.GetRows("Sheet1")
	f.must(err)
	last := len(rows[0]) - 1
	if rows[0][last] != "Ответ на вопрос «Трансфер <&>» (№"+strconv.FormatInt(q, 10)+")" {
		t.Fatal(rows[0])
	}
	for _, row := range rows[1:] {
		if row[0] == "42" && (len(row) <= last || row[last] != "Да <&>") {
			t.Fatal("wrong exported answer", row)
		}
	}
	f.send(1, "/question_close "+strconv.FormatInt(q, 10), "")
	f.send(51, "", fmt.Sprintf("qa_%d_%d", q, options[1]))
	if f.answer(q, 51) != options[0] {
		t.Fatal("closed answer changed")
	}
	f.seeded(52, "new")
	_, v = f.send(52, "/start", "")
	if v.Kind != "confirm" {
		t.Fatal("closed question still required", v)
	}
	only := f.create("Только рассылка", false)
	f.send(1, "/question_publish "+strconv.FormatInt(only, 10), "")
	f.seeded(53, "new")
	_, v = f.send(53, "/start", "")
	if v.Kind != "confirm" {
		t.Fatal("broadcast-only question blocks registration")
	}
	onlyOptions := f.options(only)
	_, v = f.send(51, "", fmt.Sprintf("qa_%d_%d", only, onlyOptions[0]))
	if v.Kind != "registered" || len(v.Fields) != 5 {
		t.Fatal("broadcast answer exposed", v)
	}
	f.svc = &service.Service{DB: f.db, BotID: 1000, RootID: 1}
	_, v = f.send(51, "/start", "")
	if v.Kind != "registered" || f.answer(only, 51) != onlyOptions[0] {
		t.Fatal("answer did not survive restart", v)
	}
}

func TestSurveyPublicationAndConfirmationRace(t *testing.T) {
	f := newSurveyFixture(t, "survey_race_test")
	f.send(1, "/start", "")
	for i := range 5 {
		actor := int64(100 + i)
		f.seeded(actor, "confirm")
		q := f.create(fmt.Sprintf("Race %d", i), true)
		// Earlier questions must be answered, so only the new publication is racing.
		f.exec(`INSERT INTO survey_answers(question_id,telegram_id,option_id)
    SELECT q.id,$1,min(o.id) FROM survey_questions q JOIN survey_options o ON o.question_id=q.id
    WHERE q.status='open' GROUP BY q.id`, actor)
		var version int64
		f.must(f.db.QueryRow(f.ctx, "SELECT version FROM users WHERE telegram_id=$1", actor).Scan(&version))
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, in := range []*pb.Update{
			{Actor: 1, Chat: 1, Text: "/question_publish " + strconv.FormatInt(q, 10)},
			{Actor: actor, Chat: actor, Callback: fmt.Sprintf("c:%d:confirm", version)},
		} {
			wg.Go(func() {
				<-start
				in.Id = f.seq.Add(1)
				in.ChatType = "private"
				_, err := f.svc.Accept(f.ctx, in)
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			f.must(err)
		}
		var state string
		var targeted bool
		f.must(f.db.QueryRow(f.ctx, `SELECT state,EXISTS(SELECT 1 FROM outbound_messages m JOIN survey_questions q ON q.broadcast_id=m.broadcast_id WHERE q.id=$1 AND m.chat=$2) FROM users WHERE telegram_id=$2`, q, actor).Scan(&state, &targeted))
		if state == "registered" && !targeted {
			t.Fatal("publication missed a concurrent registration")
		}
		if !strings.HasPrefix(state, "survey_") && state != "registered" {
			t.Fatal(state)
		}
	}
}

func TestSurveyCloseAndAnswerRace(t *testing.T) {
	f := newSurveyFixture(t, "survey_close_test")
	f.seeded(42, "registered")
	for i := range 10 {
		q := f.create(fmt.Sprintf("Close %d", i), true)
		options := f.options(q)
		f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, in := range []*pb.Update{
			{Actor: 1, Chat: 1, Text: "/question_close " + strconv.FormatInt(q, 10)},
			{Actor: 42, Chat: 42, Callback: fmt.Sprintf("qa_%d_%d", q, options[0]), MessageId: 700, CallbackMessageEditable: true},
		} {
			wg.Go(func() {
				<-start
				in.Id = f.seq.Add(1)
				in.ChatType = "private"
				_, err := f.svc.Accept(f.ctx, in)
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			f.must(err)
		}
		answer := f.answer(q, 42)
		_, v := f.send(42, "", fmt.Sprintf("qa_%d_%d", q, options[1]))
		if len(v.Buttons) != 0 || f.answer(q, 42) != answer {
			t.Fatal("closed question accepted an answer")
		}
	}
	extra := f.create("Beyond limit", true)
	_, v := f.send(1, "/question_publish "+strconv.FormatInt(extra, 10), "")
	if v.Code != "survey_limit" {
		t.Fatal("questionnaire bound not enforced", v)
	}
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM broadcasts").Scan(&count))
	if count != 10 {
		t.Fatal("failed publication created a broadcast", count)
	}
}

func TestSurveyAnswerAndReceiptAtomicity(t *testing.T) {
	f := newSurveyFixture(t, "survey_atomicity_test")
	f.seeded(42, "registered")
	q := f.create("Atomic", true)
	option := f.options(q)[0]
	f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
	f.exec("ALTER TABLE outbound_messages ADD CONSTRAINT fixture_receipt_failure CHECK(body->>'kind'<>'registered') NOT VALID")
	update := &pb.Update{Id: f.seq.Add(1), Actor: 42, Chat: 42, ChatType: "private", MessageId: 700, CallbackMessageEditable: true, Callback: fmt.Sprintf("qa_%d_%d", q, option)}
	if _, err := f.svc.Accept(f.ctx, update); err == nil {
		t.Fatal("receipt failure was acknowledged")
	}
	if f.answer(q, 42) != 0 {
		t.Fatal("answer committed without durable receipt")
	}
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM processed_updates WHERE update_id=$1", update.Id).Scan(&count))
	if count != 0 {
		t.Fatal("failed callback was deduplicated")
	}
	f.exec("ALTER TABLE outbound_messages DROP CONSTRAINT fixture_receipt_failure")
	r, err := f.svc.Accept(f.ctx, update)
	f.must(err)
	if r.Duplicate || len(r.DeliveryIds) != 2 || f.answer(q, 42) != option {
		t.Fatal("callback not recoverable after failure", r)
	}
	clear := f.claim(r.DeliveryIds[0])
	f.complete(clear)
	f.send(1, "/question_close "+strconv.FormatInt(q, 10), "")
	d := f.claim(r.DeliveryIds[1])
	if d.View.Kind != "registered" || len(d.View.Fields) != 5 || d.EditMessageId != 0 {
		t.Fatal("receipt is not a private questionnaire", d)
	}
}

func TestSurveyDraftModeFencing(t *testing.T) {
	f := newSurveyFixture(t, "survey_draft_test")
	f.send(1, "/question_new", "")
	f.send(1, "Old", "")
	f.send(1, "Old body", "")
	_, old := f.send(1, "A\nB", "")
	f.send(1, "/question_new", "")
	f.send(1, "New", "")
	f.send(1, "New body", "")
	_, current := f.send(1, "C\nD", "")
	_, v := f.send(1, "", old.Buttons[0].Data)
	if v.Code != "survey_mode_prompt" {
		t.Fatal("stale mode should redisplay current choices", v)
	}
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM survey_questions").Scan(&count))
	if count != 0 {
		t.Fatal("stale draft button created a question")
	}
	_, v = f.send(1, "", current.Buttons[0].Data)
	f.click(1, v, "audience_registered")
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM survey_questions WHERE title='New'").Scan(&count))
	if count != 1 {
		t.Fatal("current mode not accepted")
	}
}

func TestSurveyPhotoQuestions(t *testing.T) {
	f := newSurveyFixture(t, "survey_photos_test")
	f.seeded(42, "registered")
	f.send(1, "/question_new", "")
	f.send(1, "Photo question", "")
	// Invalid captions keep the wizard at the same step instead of losing the photo.
	_, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", PhotoFileId: "fixture-photo", PhotoCaption: strings.Repeat("😀", 513)})
	f.must(err)
	var stage string
	f.must(f.db.QueryRow(f.ctx, "SELECT stage FROM survey_drafts WHERE actor=1").Scan(&stage))
	if stage != "body" {
		t.Fatal("oversized caption consumed the wizard step")
	}
	_, err = f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", PhotoFileId: "fixture-photo", PhotoCaption: "<Фото> & вопрос?"})
	f.must(err)
	_, mode := f.send(1, "Да\nНет", "")
	// Restart between accepting the forwarded photo and finishing the wizard.
	f.svc = &service.Service{DB: f.db, RootID: 1, BotID: 1000}
	_, audience := f.send(1, "", mode.Buttons[0].Data)
	r, v := f.send(1, "", audience.Buttons[1].Data)
	q := v.Numbers[0]
	options := f.options(q)
	var body []byte
	f.must(f.db.QueryRow(f.ctx, "SELECT body FROM outbound_messages WHERE id=ANY($1) AND body->>'kind'='survey_preview'", r.DeliveryIds).Scan(&body))
	preview := &pb.View{}
	f.must(protojson.Unmarshal(body, preview))
	if preview.PhotoFileId != "fixture-photo" || preview.Text != "<Фото> & вопрос?" || len(preview.Buttons) != 2 {
		t.Fatal("photo lost from preview", preview)
	}
	f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
	var job int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE chat=42 AND broadcast_id IS NOT NULL").Scan(&job))
	d := f.claim(job)
	if d.View.PhotoFileId != "fixture-photo" {
		t.Fatal("photo lost from broadcast")
	}
	f.complete(d)
	input := &pb.Update{Id: f.seq.Add(1), Actor: 42, Chat: 42, ChatType: "private", MessageId: 700, CallbackPhotoEditable: true, Callback: fmt.Sprintf("qa_%d_%d", q, options[0])}
	r, err = f.svc.Accept(f.ctx, input)
	f.must(err)
	if len(r.DeliveryIds) != 2 || f.answer(q, 42) != options[0] {
		t.Fatal("photo answer not durable", r)
	}
	clear := f.claim(r.DeliveryIds[0])
	if clear.Kind != "survey_remove_keyboard" || clear.SourceMessage != 700 {
		t.Fatal("photo keyboard not removed", clear)
	}
	f.complete(clear)
	receipt := f.claim(r.DeliveryIds[1])
	if receipt.View.PhotoFileId != "" || receipt.EditMessageId != 0 || receipt.View.Kind != "registered" || len(receipt.View.Fields) != 5 {
		t.Fatal("photo receipt tried to replace the caption", receipt)
	}
	f.complete(receipt)
	_, v = f.send(42, "/start", "")
	v = f.click(42, v, "edit")
	for _, button := range v.Buttons {
		if strings.Contains(button.Data, "survey_") {
			t.Fatal("photo answer offered for editing")
		}
	}
	var version int64
	f.must(f.db.QueryRow(f.ctx, "SELECT version FROM users WHERE telegram_id=42").Scan(&version))
	f.send(42, "", fmt.Sprintf("c:%d:edit_survey_%d", version, q))
	if f.answer(q, 42) != options[0] {
		t.Fatal("photo answer changed through forged editor button")
	}

	// New registrations get the same photo before their final confirmation.
	f.seeded(51, "new")
	_, v = f.send(51, "/start", "")
	if v.Kind != "survey_question" || v.PhotoFileId != "fixture-photo" {
		t.Fatal("new registration lost the photo", v)
	}
	input = &pb.Update{Id: f.seq.Add(1), Actor: 51, Chat: 51, ChatType: "private", MessageId: 702, CallbackPhotoEditable: true, Callback: v.Buttons[0].Data}
	r, err = f.svc.Accept(f.ctx, input)
	f.must(err)
	f.must(f.db.QueryRow(f.ctx, "SELECT body FROM outbound_messages WHERE id=$1", r.DeliveryIds[len(r.DeliveryIds)-1]).Scan(&body))
	final := &pb.View{}
	f.must(protojson.Unmarshal(body, final))
	if final.Kind != "confirm" || final.PhotoFileId != "" || f.answer(q, 51) != options[0] {
		t.Fatal("photo registration answer did not advance", final)
	}
	var firstKind string
	f.must(f.db.QueryRow(f.ctx, "SELECT kind FROM outbound_messages WHERE id=$1", r.DeliveryIds[0]).Scan(&firstKind))
	if firstKind != "survey_remove_keyboard" {
		t.Fatal("last photo question retained its buttons")
	}
}

func TestSurveyPhotoBeforeTitle(t *testing.T) {
	f := newSurveyFixture(t, "survey_photo_first_test")
	for i, caption := range []string{"Forwarded caption <&>", ""} {
		f.send(1, "/question_new", "")
		r, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", PhotoFileId: "fixture-photo", PhotoCaption: caption})
		f.must(err)
		var body []byte
		f.must(f.db.QueryRow(f.ctx, "SELECT body FROM outbound_messages WHERE id=$1", r.DeliveryIds[0]).Scan(&body))
		v := &pb.View{}
		f.must(protojson.Unmarshal(body, v))
		if v.Code != "survey_photo_title_prompt" {
			t.Fatal("photo not accepted before title", v)
		}
		_, v = f.send(1, fmt.Sprintf("Question %d", i), "")
		if v.Code != "survey_options_prompt" {
			t.Fatal("wizard asked for content again", v)
		}
		_, v = f.send(1, "Yes\nNo", "")
		_, v = f.send(1, "", v.Buttons[0].Data)
		v = f.click(1, v, "audience_registered")
		var photo, saved string
		f.must(f.db.QueryRow(f.ctx, "SELECT photo_file_id,body FROM survey_questions WHERE id=$1", v.Numbers[0]).Scan(&photo, &saved))
		expected := caption
		if expected == "" {
			expected = fmt.Sprintf("Question %d", i)
		}
		if photo != "fixture-photo" || saved != expected {
			t.Fatal("photo or caption lost before title")
		}
	}
}

func TestSurveyUpgradeFromDeployedV5(t *testing.T) {
	f := newSurveyFixture(t, "survey_upgrade_v5_test")
	// Reconstruct the deployed schema without touching any existing v1-v5 object.
	f.exec("DROP VIEW survey_participants; DROP TABLE survey_answers,survey_options,survey_questions,survey_drafts; DELETE FROM schema_migrations WHERE version>=6")
	if store.CheckSchema(f.ctx, f.db) == nil {
		t.Fatal("v5 must require the new migration")
	}
	f.seeded(42, "registered")
	f.exec("INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES(42,'table_viewer',1)")
	f.exec("INSERT INTO first_starts(telegram_id,source,started_at) VALUES(42,'synthetic_source',now())")
	f.exec("INSERT INTO bot_chats(chat_id,chat_type,chat_title) VALUES(-100,'staff','Synthetic staff')")
	f.exec("INSERT INTO outbound_messages(chat,priority,kind,body,status,attempts) VALUES(42,'interactive','view','{\"kind\":\"registered\"}','retry_wait',2)")
	snapshot := func() string {
		t.Helper()
		var value string
		f.must(f.db.QueryRow(f.ctx, `SELECT jsonb_build_object(
   'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY telegram_id) FROM users u),
   'permissions',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM user_permissions p),
   'chats',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM bot_chats c),
   'sources',(SELECT jsonb_agg(to_jsonb(s) ORDER BY telegram_id) FROM first_starts s),
   'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM outbound_messages o),
   'history',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<=5)
  )::text`).Scan(&value))
		return value
	}
	before := snapshot()
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	if snapshot() != before {
		t.Fatal("upgrade changed existing data or migration history")
	}
	f.must(store.Migrate(f.ctx, f.db))
	if snapshot() != before {
		t.Fatal("repeat migration changed existing data")
	}
	var version, history int
	f.must(f.db.QueryRow(f.ctx, "SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &history))
	if version != 7 || history != 7 {
		t.Fatal("expected complete migration history", version, history)
	}
	q := f.create("Photo in one migration", true)
	var photo string
	f.must(f.db.QueryRow(f.ctx, "SELECT photo_file_id FROM survey_questions WHERE id=$1", q).Scan(&photo))
	if photo != "" {
		t.Fatal("unexpected default photo")
	}
}

func TestAnsweredSurveyBroadcastIsNotResent(t *testing.T) {
	f := newSurveyFixture(t, "survey_no_resend_test")
	f.seeded(42, "registered")
	q := f.create("No duplicates", true)
	option := f.options(q)[0]
	f.send(1, "/question_publish "+strconv.FormatInt(q, 10), "")
	var job int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE chat=42 AND broadcast_id IS NOT NULL").Scan(&job))
	f.send(42, "", fmt.Sprintf("qa_%d_%d", q, option))
	d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "survey-fixture-worker"})
	f.must(err)
	if d.Id != 0 {
		t.Fatal("answered question was delivered again", d)
	}
	var state string
	f.must(f.db.QueryRow(f.ctx, "SELECT status FROM outbound_messages WHERE id=$1", job).Scan(&state))
	if state != "cancelled" {
		t.Fatal("obsolete invitation was not cancelled")
	}
}

func TestSurveyBroadcastAudiences(t *testing.T) {
	f := newSurveyFixture(t, "survey_audiences_test")
	f.svc.ParticipationEnabled = true
	for _, actor := range []int64{1, 42, 44, 45, 46} {
		f.seeded(actor, "registered")
	}
	f.seeded(43, "name")
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id IN (44,46)")
	f.exec("UPDATE users SET is_counselor=1 WHERE telegram_id=45")
	f.exec("UPDATE users SET is_blocked=1 WHERE telegram_id=46")
	f.exec("UPDATE users SET will_drive='Обязательно! 🤩' WHERE telegram_id=42")
	f.exec("UPDATE users SET will_drive='Пока думаю 🤔' WHERE telegram_id=43")
	var staffQuestion int64
	for _, tc := range []struct {
		audience string
		want     []int64
	}{
		{"all", []int64{1, 42, 43, 44, 45}},
		{"registered", []int64{1, 42, 44, 45}},
		{"incomplete", []int64{43}},
		{"staff", []int64{44}},
		{"counselor", []int64{45}},
		{"non_counselor", []int64{1, 42, 43, 44}},
		{"yes", []int64{42}},
		{"maybe", []int64{43}},
	} {
		q := f.createAudience(tc.audience, false, tc.audience)
		_, preview := f.send(1, fmt.Sprintf("/question_preview %d", q), "")
		if preview.Text != tc.audience || preview.Numbers[1] != int64(len(tc.want)) {
			t.Fatal("incorrect audience preview", preview)
		}
		f.send(1, fmt.Sprintf("/question_publish %d", q), "")
		f.send(1, fmt.Sprintf("/question_publish %d", q), "")
		var got []int64
		var audience string
		f.must(f.db.QueryRow(f.ctx, `SELECT b.audience,ARRAY(SELECT chat FROM outbound_messages WHERE broadcast_id=b.id ORDER BY chat) FROM survey_questions q JOIN broadcasts b ON b.id=q.broadcast_id WHERE q.id=$1`, q).Scan(&audience, &got))
		if audience != tc.audience || !slices.Equal(got, tc.want) {
			t.Fatal(tc.audience, got, tc.want)
		}
		f.must(f.db.QueryRow(f.ctx, "SELECT ARRAY(SELECT telegram_id FROM survey_participants WHERE question_id=$1 ORDER BY telegram_id)", q).Scan(&got))
		if !slices.Equal(got, tc.want) {
			t.Fatal("results include people outside audience", tc.audience, got)
		}
		if tc.audience == "staff" {
			staffQuestion = q
		}
	}
	// Changing group membership later must not change the publication snapshot.
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id=42")
	option := f.options(staffQuestion)[0]
	f.send(42, "", fmt.Sprintf("qa_%d_%d", staffQuestion, option))
	if f.answer(staffQuestion, 42) != 0 {
		t.Fatal("accepted a callback from outside the original audience")
	}
	f.send(44, "", fmt.Sprintf("qa_%d_%d", staffQuestion, option))
	if f.answer(staffQuestion, 44) != option {
		t.Fatal("targeted answer was lost")
	}
	// Form inclusion remains independent of the broadcast audience.
	f.svc.ParticipationEnabled = false
	q := f.createAudience("Staff and new participants", true, "counselor")
	f.send(1, fmt.Sprintf("/question_publish %d", q), "")
	f.seeded(50, "new")
	_, v := f.send(50, "/start", "")
	if v.Kind != "survey_question" || v.Numbers[0] != q {
		t.Fatal("form inclusion incorrectly filtered by audience", v)
	}
}

func TestSurveyAudienceDraftFencing(t *testing.T) {
	f := newSurveyFixture(t, "survey_audience_draft_test")
	prompt := func() *pb.View {
		f.send(1, "/question_new", "")
		f.send(1, "Question", "")
		f.send(1, "Body", "")
		_, v := f.send(1, "A\nB", "")
		_, v = f.send(1, "", v.Buttons[1].Data)
		return v
	}
	old := prompt()
	current := prompt()
	// Durable draft survives process restart, rejects stale or forged audience callbacks.
	f.svc = &service.Service{DB: f.db, BotID: 1000, RootID: 1}
	for _, callback := range []string{old.Buttons[0].Data, strings.Replace(current.Buttons[0].Data, ":all", ":invalid", 1), strings.Replace(current.Buttons[0].Data, ":all", ":yes", 1)} {
		_, v := f.send(1, "", callback)
		if v.Code != "survey_audience_prompt" {
			t.Fatal("invalid audience advanced draft", v)
		}
		for _, b := range v.Buttons {
			if b.LabelKey == "audience_yes" || b.LabelKey == "audience_maybe" {
				t.Fatal("disabled participation audience offered")
			}
		}
	}
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM survey_questions").Scan(&count))
	if count != 0 {
		t.Fatal("stale or forged audience created question")
	}
	v := f.click(1, current, "audience_staff")
	if v.Text != "staff" {
		t.Fatal("current audience not accepted", v)
	}
	// Empty audiences can be previewed and published without accidental broadening.
	f.send(1, fmt.Sprintf("/question_publish %d", v.Numbers[0]), "")
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id IS NOT NULL").Scan(&count))
	if count != 0 {
		t.Fatal("empty audience expanded to all users")
	}
}

func TestSurveyUpgradeFromDeployedV6(t *testing.T) {
	f := newSurveyFixture(t, "survey_upgrade_v6_test")
	f.seeded(42, "registered")
	q := f.create("Existing", false)
	f.send(1, fmt.Sprintf("/question_publish %d", q), "")
	option := f.options(q)[0]
	f.send(42, "", fmt.Sprintf("qa_%d_%d", q, option))
	f.send(1, "/question_new", "")
	f.send(1, "Old draft", "")
	f.send(1, "Body", "")
	_, mode := f.send(1, "A\nB", "")
	// Reconstruct schema 006 in this disposable fixture, leaving its feature data intact.
	f.exec("ALTER TABLE survey_questions DROP COLUMN audience; ALTER TABLE survey_drafts DROP COLUMN in_registration; ALTER TABLE survey_drafts DROP CONSTRAINT survey_drafts_stage_check; ALTER TABLE survey_drafts ADD CONSTRAINT survey_drafts_stage_check CHECK(stage IN ('title','body','options','mode')); DELETE FROM schema_migrations WHERE version=7")
	var before string
	const history = "SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<=6"
	f.must(f.db.QueryRow(f.ctx, history).Scan(&before))
	if store.CheckSchema(f.ctx, f.db) == nil {
		t.Fatal("v6 must require migration 007")
	}
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	var after, audience string
	f.must(f.db.QueryRow(f.ctx, history).Scan(&after))
	f.must(f.db.QueryRow(f.ctx, "SELECT audience FROM survey_questions WHERE id=$1", q).Scan(&audience))
	if before != after || audience != "registered" || f.answer(q, 42) != option {
		t.Fatal("upgrade changed existing question, answer or migration history")
	}
	_, v := f.send(1, "", mode.Buttons[0].Data)
	if v.Code != "survey_audience_prompt" {
		t.Fatal("old draft cannot continue", v)
	}
	v = f.click(1, v, "audience_staff")
	var registration bool
	f.must(f.db.QueryRow(f.ctx, "SELECT in_registration FROM survey_questions WHERE id=$1", v.Numbers[0]).Scan(&registration))
	if !registration {
		t.Fatal("old draft lost form inclusion")
	}
}

func TestSurveyExportReadableAnswers(t *testing.T) {
	f := newSurveyFixture(t, "survey_export_labels_test")
	for _, actor := range []int64{42, 43, 44} {
		f.seeded(actor, "registered")
	}
	first := f.create("Участие", false)
	second := f.create("Участие", false)
	f.create("Неопубликованный вопрос", false)
	firstOptions, secondOptions := f.options(first), f.options(second)
	// Formula-looking content remains plain text, including Unicode and HTML characters.
	const label = "=ДА & <готов> 👍"
	f.exec("UPDATE survey_options SET label=$2 WHERE id=$1", firstOptions[0], label)
	for _, id := range []int64{first, second} {
		f.send(1, fmt.Sprintf("/question_publish %d", id), "")
	}
	f.send(42, "", fmt.Sprintf("qa_%d_%d", first, firstOptions[0]))
	f.send(42, "", fmt.Sprintf("qa_%d_%d", second, secondOptions[1]))
	f.send(43, "", fmt.Sprintf("qa_%d_%d", second, secondOptions[0]))
	f.send(1, fmt.Sprintf("/question_close %d", second), "")
	for _, participation := range []bool{false, true} {
		f.svc.ParticipationEnabled = participation
		exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
		f.must(err)
		file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
		f.must(err)
		rows, err := file.GetRows("Sheet1")
		f.must(err)
		start := 14
		if participation {
			start = 16
		}
		if len(rows[0]) != start+2 || rows[0][start] != fmt.Sprintf("Ответ на вопрос «Участие» (№%d)", first) || rows[0][start+1] != fmt.Sprintf("Ответ на вопрос «Участие» (№%d)", second) {
			t.Fatal("questions missing, ambiguous, misplaced, or include a draft", rows[0])
		}
		want := map[string][]string{"42": {label, "Нет"}, "43": {"", "Да <&>"}, "44": {"", ""}}
		seen := map[string]bool{}
		for rowIndex, row := range rows[1:] {
			answers, ok := want[row[0]]
			if !ok {
				continue
			}
			seen[row[0]] = true
			for i, answer := range answers {
				cell, err := excelize.CoordinatesToCellName(start+i+1, rowIndex+2)
				f.must(err)
				actual, err := file.GetCellValue("Sheet1", cell)
				f.must(err)
				formula, err := file.GetCellFormula("Sheet1", cell)
				f.must(err)
				if actual != answer || formula != "" {
					t.Fatalf("actor %s, question %d: got %q, want %q, formula %q", row[0], i, actual, answer, formula)
				}
			}
		}
		if len(seen) != len(want) {
			t.Fatal("export lost participants", seen)
		}
		f.must(file.Close())
	}
}
