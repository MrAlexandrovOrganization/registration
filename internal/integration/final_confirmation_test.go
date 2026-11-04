//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

// Reconstructing an older schema is restricted to the disposable test database.
func dropFinalConfirmationSchema(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	if db.Config().ConnConfig.Database != "registration_test" {
		t.Fatal("requires disposable registration_test")
	}
	_, err := db.Exec(t.Context(), `DROP VIEW final_confirmation_eligible;
DROP TABLE final_confirmation_invitations,final_confirmation_campaign,final_confirmation_drafts;
ALTER TABLE users DROP COLUMN registration_completed_at;
DELETE FROM schema_migrations WHERE version>=8`)
	if err != nil {
		t.Fatal(err)
	}
}

func seedFinalConfirmation(f *surveyFixture, count int) (int64, int64) {
	f.t.Helper()
	question := f.create("agreement_1", false)
	positive := f.options(question)[0]
	f.exec("UPDATE survey_questions SET status='open' WHERE id=$1", question)
	// ID and created/updated order are the opposite of completion order.
	f.exec(`INSERT INTO users(telegram_id,state,name,birth_date,"group",phone,expectations,has_registered,registration_completed_at,updated_at)
SELECT 10000-n,'registered','Synthetic','01.01.2000','ИУ7-41','79991234567','Synthetic',true,
 '2026-10-01'::timestamptz+n*interval '1 minute','2026-11-01'::timestamptz-n*interval '1 minute'
FROM generate_series(1,$1::int) AS g(n)`, count)
	f.exec(`INSERT INTO survey_answers(question_id,telegram_id,option_id)
SELECT $1,telegram_id,$2 FROM users WHERE telegram_id BETWEEN 10000-$3::int AND 9999`, question, positive, count)
	return question, positive
}

func finalCount(f *surveyFixture, where string) int {
	f.t.Helper()
	var count int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM final_confirmation_invitations WHERE "+where).Scan(&count))
	return count
}

func prepareFinalConfirmation(f *surveyFixture, question, positive int64) int64 {
	f.t.Helper()
	f.send(1, fmt.Sprintf("/final_new %d %d", question, positive), "")
	_, prompt := f.send(1, "Synthetic forwarded invitation <&>", "")
	if prompt.Code != "final_buttons_prompt" {
		f.t.Fatal("button names were not requested", prompt)
	}
	r, preview := f.send(1, "Да, еду ✅\nНет, не смогу 😢", "")
	if preview.Code != "final_preview_info" || len(r.DeliveryIds) != 2 || finalCount(f, "true") != 0 {
		f.t.Fatal("preparation did not produce a preview without inviting participants", preview)
	}
	deliverFinalPreview(f, r.DeliveryIds[0], 700)
	var id int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM final_confirmation_drafts WHERE actor=1").Scan(&id))
	return id
}

// Deliver preceding private operator output through the real Claim/Complete
// contract before acknowledging the preview, preserving chat order.
func deliverFinalPreview(f *surveyFixture, lastID, messageID int64) *pb.Delivery {
	f.t.Helper()
	rows, err := f.db.Query(f.ctx, "SELECT id FROM outbound_messages WHERE chat=1 AND id<=$1 AND status='pending' ORDER BY id", lastID)
	f.must(err)
	var ids []int64
	for rows.Next() {
		var id int64
		f.must(rows.Scan(&id))
		ids = append(ids, id)
	}
	f.must(rows.Err())
	rows.Close()
	var last *pb.Delivery
	for _, id := range ids {
		last = f.claim(id)
		_, err = f.svc.Complete(f.ctx, &pb.Completion{Id: id, Lease: last.Lease, Outcome: "sent", TelegramMessageId: messageID})
		f.must(err)
	}
	_, err = f.svc.ReleaseSender(f.ctx, &pb.ReleaseSenderRequest{Worker: "survey-fixture-worker"})
	f.must(err)
	return last
}

func TestFinalConfirmationForwardedPreviewAndStart(t *testing.T) {
	f := newSurveyFixture(t, "final_forwarded_preview_test")
	q, positive := seedFinalConfirmation(f, 3)
	_, prompt := f.send(1, fmt.Sprintf("/final_new %d %d", q, positive), "")
	if prompt.Code != "final_send_source" {
		t.Fatal("message was not requested", prompt)
	}
	r, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", Kind: "message", MessageId: 350, PhotoFileId: "synthetic-photo", PhotoCaption: "<Custom caption>"})
	f.must(err)
	if len(r.DeliveryIds) != 1 {
		t.Fatal("a forwarded message should request labels before previewing")
	}
	var draft int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM final_confirmation_drafts WHERE actor=1").Scan(&draft))
	start := fmt.Sprintf("/final_start %d", draft)
	_, v := f.send(1, start, "")
	if v.Code != "final_buttons_prompt" || finalCount(f, "true") != 0 {
		t.Fatal("started without explicit button names", v)
	}
	for _, invalid := range []string{"Одна кнопка", "Да\nДа", "Да\nНет\nВозможно", strings.Repeat("😀", 31) + "\nНет"} {
		_, v = f.send(1, invalid, "")
		if v.Code != "final_buttons_prompt" {
			t.Fatal("invalid labels were accepted", v)
		}
	}
	r, _ = f.send(1, "  Точно буду ✅  \r\n Не смогу 😢 ", "")
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM final_confirmation_drafts WHERE actor=1").Scan(&draft))
	start = fmt.Sprintf("/final_start %d", draft)
	_, v = f.send(1, start, "")
	if v.Code != "final_preview_pending" || finalCount(f, "true") != 0 {
		t.Fatal("started before preview delivery", v)
	}
	d := deliverFinalPreview(f, r.DeliveryIds[0], 777)
	if d.Kind != "copy" || d.SourceChat != 1 || d.SourceMessage != 350 || d.View.Kind != "final_preview" || !strings.HasPrefix(d.View.Buttons[0].Data, "preview_final_") {
		t.Fatal("preview lost forwarded source or used live answer buttons", d)
	}
	if d.View.Buttons[0].Label != "Точно буду ✅" || d.View.Buttons[1].Label != "Не смогу 😢" {
		t.Fatal("preview lost or changed the custom button names", d.View.Buttons)
	}
	// Preview callbacks never record a final answer or invite anybody.
	f.send(1, "", d.View.Buttons[0].Data)
	if finalCount(f, "true") != 0 {
		t.Fatal("preview button allocated invitations")
	}
	r, _ = f.send(1, fmt.Sprintf("/final_preview %d", draft), "")
	var source int64
	f.must(f.db.QueryRow(f.ctx, "SELECT source_message FROM outbound_messages WHERE id=$1", r.DeliveryIds[0]).Scan(&source))
	if source != 777 {
		t.Fatal("repeated preview did not use the frozen bot-owned copy")
	}
	f.send(1, start, "")
	var invitations int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE body->>'kind'='notice' AND body->>'code'='final_invitation' AND kind='copy' AND source_chat=1 AND source_message=777 AND body->'buttons'->0->>'label'=$1 AND body->'buttons'->1->>'label'=$2", "Точно буду ✅", "Не смогу 😢").Scan(&invitations))
	if invitations != 3 || finalCount(f, "true") != 3 {
		t.Fatal("campaign did not copy exactly the reviewed invitation")
	}
}

func TestFinalConfirmationReplacedAndCancelledPreview(t *testing.T) {
	f := newSurveyFixture(t, "final_replaced_preview_test")
	q, positive := seedFinalConfirmation(f, 3)
	newDraft := fmt.Sprintf("/final_new %d %d", q, positive)
	f.send(1, newDraft, "")
	f.send(1, "First forwarded message", "")
	r, _ := f.send(1, "Первое подтверждение\nПервый отказ", "")
	var oldID int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM final_confirmation_drafts WHERE actor=1").Scan(&oldID))
	// An old preview can be in flight when the operator starts a replacement.
	f.exec("UPDATE outbound_messages SET status='sent' WHERE chat=1 AND id<$1", r.DeliveryIds[0])
	old := f.claim(r.DeliveryIds[0])
	f.send(1, newDraft, "")
	f.send(1, "Replacement forwarded message", "")
	r, _ = f.send(1, "Новое подтверждение\nНовый отказ", "")
	var currentID int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM final_confirmation_drafts WHERE actor=1").Scan(&currentID))
	f.complete(old)
	_, err := f.svc.ReleaseSender(f.ctx, &pb.ReleaseSenderRequest{Worker: "survey-fixture-worker"})
	f.must(err)
	_, v := f.send(1, fmt.Sprintf("/final_start %d", oldID), "")
	if v.Code != "final_draft_not_found" {
		t.Fatal("stale preview could be approved", v)
	}
	_, v = f.send(1, fmt.Sprintf("/final_start %d", currentID), "")
	if v.Code != "final_preview_pending" || finalCount(f, "true") != 0 {
		t.Fatal("late completion approved the replacement message", v)
	}
	deliverFinalPreview(f, r.DeliveryIds[0], 888)
	f.send(1, "/cancel", "")
	_, v = f.send(1, fmt.Sprintf("/final_start %d", currentID), "")
	if v.Code != "final_draft_not_found" || finalCount(f, "true") != 0 {
		t.Fatal("cancelled preview could be started", v)
	}
}

func TestFinalConfirmationCapacityAndDecline(t *testing.T) {
	f := newSurveyFixture(t, "final_capacity_test")
	q, positive := seedFinalConfirmation(f, 100)
	// Every exclusion predicate is exercised even with a prior positive agreement.
	for i, flag := range []string{"is_blocked=1", "is_staff=1", "is_counselor=1", "has_registered=false"} {
		f.seeded(int64(50+i), "registered")
		f.exec("UPDATE users SET registration_completed_at='2026-01-01',"+flag+" WHERE telegram_id=$1", 50+i)
		f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,$2,$3)", q, 50+i, positive)
	}
	f.seeded(60, "registered") // A negative agreement never qualifies.
	f.exec("UPDATE users SET registration_completed_at='2026-01-01' WHERE telegram_id=60")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,60,$2)", q, f.options(q)[1])
	f.seeded(61, "registered") // Neither does a missing agreement.
	f.exec("UPDATE users SET registration_completed_at='2026-01-01' WHERE telegram_id=61")
	// A completed form being edited still qualifies, preserving its original rank.
	f.exec("UPDATE users SET state='edit_name' WHERE telegram_id=9999")
	draft := prepareFinalConfirmation(f, q, positive)
	start := fmt.Sprintf("/final_start %d", draft)
	f.send(60, start, "") // Ordinary participants cannot launch a campaign.
	if finalCount(f, "true") != 0 {
		t.Fatal("participant started confirmation")
	}
	f.send(1, start, "")
	f.send(1, start, "") // A repeat is idempotent.
	if finalCount(f, "true") != 90 || finalCount(f, "telegram_id BETWEEN 9910 AND 9999") != 90 {
		t.Fatal("wrong initial audience or order")
	}
	var deliveries int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE body->>'kind'='notice' AND body->>'code'='final_invitation'").Scan(&deliveries))
	if deliveries != 90 {
		t.Fatal("repeat start duplicated invitations", deliveries)
	}
	// A waitlisted participant cannot take a place by constructing a callback.
	f.send(9909, "", "final:yes")
	if finalCount(f, "status='yes'") != 0 {
		t.Fatal("uninvited actor accepted")
	}
	f.send(9999, "", "final:yes")
	if finalCount(f, "status='yes'") != 1 || finalCount(f, "true") != 90 {
		t.Fatal("yes released a reserved place")
	}
	// Five concurrent declines must allocate exactly the next five people.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for actor := int64(9998); actor >= 9994; actor-- {
		wg.Go(func() {
			_, err := f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: actor, Chat: actor, ChatType: "private", Callback: "final:no"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		f.must(err)
	}
	if finalCount(f, "status IN ('invited','yes')") != 90 || finalCount(f, "status='no'") != 5 || finalCount(f, "telegram_id BETWEEN 9905 AND 9909") != 5 {
		t.Fatal("concurrent declines violated capacity or queue order")
	}
	// A duplicate update replays exactly its original durable reply IDs.
	update := &pb.Update{Id: f.seq.Add(1), Actor: 9993, Chat: 9993, ChatType: "private", Callback: "final:no", MessageId: 700, CallbackMessageEditable: true}
	r, err := f.svc.Accept(f.ctx, update)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, update)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(r.DeliveryIds, replay.DeliveryIds) {
		t.Fatal("decline replay changed receipts")
	}
	f.send(9993, "", "final:yes")
	f.send(9993, "", "final:no")
	if finalCount(f, "status='no'") != 6 || finalCount(f, "true") != 96 {
		t.Fatal("an old button changed a decline or issued another invitation")
	}
	// An outbox failure rolls back the decline and replacement together. The
	// same inbound update can then be retried without losing the place or reply.
	f.exec("ALTER TABLE outbound_messages ADD CONSTRAINT fail_final_replacement CHECK(body->>'code' IS DISTINCT FROM 'final_invitation') NOT VALID")
	rollbackUpdate := &pb.Update{Id: f.seq.Add(1), Actor: 9992, Chat: 9992, ChatType: "private", Callback: "final:no"}
	_, err = f.svc.Accept(f.ctx, rollbackUpdate)
	if err == nil || finalCount(f, "status='no'") != 6 || finalCount(f, "true") != 96 {
		t.Fatal("failed replacement partially committed")
	}
	f.exec("ALTER TABLE outbound_messages DROP CONSTRAINT fail_final_replacement")
	r, err = f.svc.Accept(f.ctx, rollbackUpdate)
	f.must(err)
	if r.Duplicate || finalCount(f, "status='no'") != 7 || finalCount(f, "true") != 97 {
		t.Fatal("failed decline could not be retried atomically")
	}
	// Declined/answered invitations are cancelled at Claim, including delayed retries.
	var job int64
	f.must(f.db.QueryRow(f.ctx, "SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9993").Scan(&job))
	d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "final-test-worker"})
	f.must(err)
	if d.Id != 0 {
		t.Fatal("answered invitation regained its buttons")
	}
	var customDeliveries int
	f.must(f.db.QueryRow(f.ctx, `SELECT count(*) FROM outbound_messages WHERE body->>'kind'='notice'
AND body->>'code'='final_invitation' AND body->'buttons'->0->>'label'='Да, еду ✅'
AND body->'buttons'->1->>'label'='Нет, не смогу 😢'`).Scan(&customDeliveries))
	if customDeliveries != finalCount(f, "true") {
		t.Fatal("replacement invitations changed the approved button labels")
	}
	exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
	f.must(err)
	file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
	f.must(err)
	defer file.Close()
	rows, err := file.GetRows("Sheet1")
	f.must(err)
	col := slices.Index(rows[0], "Финальное подтверждение участия")
	if col < 0 {
		t.Fatal("final confirmation missing from export")
	}
	for _, row := range rows[1:] {
		if row[0] == "9993" && row[col] != "Нет" || row[0] == "9999" && row[col] != "Да" {
			f.t.Fatal("wrong exported final answer")
		}
	}
}

func TestFinalConfirmationDeliveryRecoveryAndBlocked(t *testing.T) {
	f := newSurveyFixture(t, "final_delivery_test")
	q, positive := seedFinalConfirmation(f, 92)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	var job int64
	f.must(f.db.QueryRow(f.ctx, "SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9999").Scan(&job))
	d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "final-delivery-worker"})
	f.must(err)
	if d.Id != job || d.View.Code != "final_invitation" || len(d.View.Buttons) != 2 {
		t.Fatal("invitation cannot be delivered", d)
	}
	_, err = f.svc.Complete(f.ctx, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: "rate_limit", RetryAfterSeconds: 2})
	f.must(err)
	f.exec("UPDATE runtime_state SET cooldown_until=now(); UPDATE outbound_messages SET next_attempt_at=now()")
	d, err = f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "final-delivery-worker"})
	f.must(err)
	_, err = f.svc.Complete(f.ctx, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: "blocked"})
	f.must(err)
	if finalCount(f, "telegram_id=9999 AND status='excluded'") != 1 || finalCount(f, "telegram_id=9909 AND status='invited'") != 1 || finalCount(f, "status IN ('invited','yes')") != 90 {
		t.Fatal("blocked delivery did not pass the invitation on")
	}
	f.exec(`UPDATE outbound_messages SET status='failed',error_code='exhausted',attempts=8
WHERE id=(SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9998);
UPDATE outbound_messages SET status='failed',error_code='content'
WHERE id=(SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9997)`)
	f.send(1, "/final_retry", "")
	var state string
	var attempts int
	f.must(f.db.QueryRow(f.ctx, "SELECT status,attempts FROM outbound_messages WHERE id=(SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9998)").Scan(&state, &attempts))
	if state != "pending" || attempts != 0 {
		t.Fatal("retry did not recover a transient failure")
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT status FROM outbound_messages WHERE id=(SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=9997)").Scan(&state))
	if state != "failed" || finalCount(f, "true") != 91 {
		t.Fatal("retry reset a content error or created extra invitations")
	}
}

func TestFinalConfirmationExhaustionAndEligibility(t *testing.T) {
	f := newSurveyFixture(t, "final_exhaustion_test")
	q, positive := seedFinalConfirmation(f, 2)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	// All candidates receive an invitation when fewer than 90 qualify.
	if finalCount(f, "true") != 2 {
		t.Fatal("small audience not filled")
	}
	f.send(9999, "", "final:no")
	f.send(9998, "", "final:no")
	if finalCount(f, "status IN ('invited','yes')") != 0 || finalCount(f, "true") != 2 {
		t.Fatal("exhausted queue repeated a declined invitation")
	}
	// A newly completed participant can fill a vacancy; edits preserve their time.
	f.seeded(42, "new")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,42,$2)", q, positive)
	f.send(42, "/start", "")
	if finalCount(f, "telegram_id=42 AND status='invited'") != 1 {
		t.Fatal("new eligible registration did not fill vacancy")
	}
	var before, after time.Time
	f.must(f.db.QueryRow(f.ctx, "SELECT registration_completed_at FROM users WHERE telegram_id=42").Scan(&before))
	f.send(42, "/start", "")
	f.must(f.db.QueryRow(f.ctx, "SELECT registration_completed_at FROM users WHERE telegram_id=42").Scan(&after))
	if !before.Equal(after) {
		t.Fatal("reopening a form changed completion order")
	}
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id=42")
	f.send(42, "", "final:yes")
	if finalCount(f, "telegram_id=42 AND status='excluded'") != 1 || finalCount(f, "status='yes'") != 0 {
		t.Fatal("ineligible participant accepted")
	}
}

func TestFinalConfirmationUpgradeAndMissingOrder(t *testing.T) {
	f := newSurveyFixture(t, "final_upgrade_test")
	q, positive := seedFinalConfirmation(f, 1)
	dropFinalConfirmationSchema(t, f.db)
	f.exec(`INSERT INTO outbound_messages(chat,actor,priority,kind,body,created_at) VALUES
(9999,9999,'interactive','view','{"kind":"confirm"}','2026-10-01'),
(9999,9999,'interactive','view','{"kind":"registered"}','2026-10-02')`)
	var before string
	f.must(f.db.QueryRow(f.ctx, "SELECT jsonb_agg(to_jsonb(o) ORDER BY id)::text FROM outbound_messages o").Scan(&before))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	var after string
	f.must(f.db.QueryRow(f.ctx, "SELECT jsonb_agg(to_jsonb(o) ORDER BY id)::text FROM outbound_messages o").Scan(&after))
	if before != after || finalCount(f, "true") != 0 {
		t.Fatal("upgrade changed history or started a campaign")
	}
	var at time.Time
	f.must(f.db.QueryRow(f.ctx, "SELECT registration_completed_at FROM users WHERE telegram_id=9999").Scan(&at))
	if !at.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("backfill did not preserve first completion")
	}
	f.exec("UPDATE users SET registration_completed_at=NULL WHERE telegram_id=9999")
	_, v := f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	if v.Code != "final_missing_order" || finalCount(f, "true") != 0 {
		t.Fatal("missing completion time was silently guessed", v)
	}
}
