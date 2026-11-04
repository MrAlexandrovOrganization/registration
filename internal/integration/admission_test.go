//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func dropAdmissionSchema(f *surveyFixture) {
	f.t.Helper()
	f.exec(`CREATE OR REPLACE VIEW final_confirmation_eligible AS
SELECT u.telegram_id,u.registration_completed_at,a.question_id,a.option_id
FROM users u JOIN survey_answers a ON a.telegram_id=u.telegram_id
WHERE u.has_registered=true AND u.is_blocked=0 AND u.is_staff=0 AND u.is_counselor=0;
ALTER TABLE survey_questions DROP COLUMN admission_closed_at;
DELETE FROM schema_migrations WHERE version>=10`)
}

func targetAdmissionSurvey(f *surveyFixture, question, actor int64) {
	f.t.Helper()
	f.exec(`INSERT INTO outbound_messages(chat,actor,priority,kind,body,broadcast_id)
SELECT $2,$2,'broadcast','view','{}',broadcast_id FROM survey_questions WHERE id=$1`, question, actor)
}

func TestAdmissionCutoffPreservesReserveAndHandlesOldButtons(t *testing.T) {
	f := newSurveyFixture(t, "admission_cutoff_test")
	q, positive := seedFinalConfirmation(f, 92)
	negative := f.options(q)[1]
	var broadcast int64
	f.must(f.db.QueryRow(f.ctx, store.Q("broadcast_create"), int64(1), "registered", "view", int64(0), int64(0)).Scan(&broadcast))
	f.exec("UPDATE survey_questions SET broadcast_id=$2 WHERE id=$1", q, broadcast)
	f.exec("UPDATE survey_questions SET in_registration=true WHERE id=$1", q)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	for _, actor := range []int64{50, 51, 54} {
		f.seeded(actor, "registered")
		f.exec("UPDATE users SET registration_completed_at='2026-01-01' WHERE telegram_id=$1", actor)
		targetAdmissionSurvey(f, q, actor)
	}
	f.seeded(52, fmt.Sprintf("survey_%d", q))
	var queuedForm int64
	f.must(f.db.QueryRow(f.ctx, `INSERT INTO outbound_messages(chat,actor,priority,kind,body)
VALUES(52,52,'interactive','view',$1) RETURNING id`, fmt.Sprintf(`{"kind":"survey_question","numbers":["%d"],"text":"Synthetic existing form"}`, q)).Scan(&queuedForm))
	f.seeded(81, "registered")
	var delayed int64
	f.must(f.db.QueryRow(f.ctx, `INSERT INTO outbound_messages(chat,actor,priority,kind,body)
VALUES(81,81,'interactive','view',$1) RETURNING id`, fmt.Sprintf(`{"kind":"survey_broadcast","numbers":["%d"]}`, q)).Scan(&delayed))
	if _, err := f.svc.CloseFinalAdmission(f.ctx, q+99); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("wrong agreement must not close admission", err)
	}
	f.exec("ALTER TABLE audit_events ADD CONSTRAINT fail_admission_audit CHECK(action <> 'final_admission_closed') NOT VALID")
	if _, err := f.svc.CloseFinalAdmission(f.ctx, q); err == nil {
		t.Fatal("failed audit must roll back closing admission")
	}
	var stillOpen bool
	f.must(f.db.QueryRow(f.ctx, "SELECT in_registration AND admission_closed_at IS NULL FROM survey_questions WHERE id=$1", q).Scan(&stillOpen))
	if !stillOpen {
		t.Fatal("failed closure partially changed admission")
	}
	f.exec("ALTER TABLE audit_events DROP CONSTRAINT fail_admission_audit")
	closed, err := f.svc.CloseFinalAdmission(f.ctx, q)
	f.must(err)
	if closed.AlreadyClosed || closed.Reserve != 2 || finalCount(f, "true") != 90 {
		t.Fatal("closing admission changed existing invitations/reserve", closed)
	}
	again, err := f.svc.CloseFinalAdmission(f.ctx, q)
	f.must(err)
	if !again.AlreadyClosed || !again.ClosedAt.Equal(closed.ClosedAt) || again.Reserve != 2 {
		t.Fatal("repeated closure moved the cutoff", again)
	}
	var inRegistration bool
	var questionStatus string
	f.must(f.db.QueryRow(f.ctx, "SELECT in_registration,status FROM survey_questions WHERE id=$1", q).Scan(&inRegistration, &questionStatus))
	if inRegistration || questionStatus != "open" {
		t.Fatal("must hide the question from forms while retaining old answers")
	}
	delivery, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: delayed, Worker: "admission-fixture"})
	f.must(err)
	if delivery.Id != 0 {
		t.Fatal("closing admission must suppress an unsent agreement")
	}
	delivery, err = f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: queuedForm, Worker: "admission-fixture"})
	f.must(err)
	if delivery.Id != queuedForm || delivery.View.Kind != "survey_question" {
		t.Fatal("an existing queued form was stranded at the cutoff")
	}
	_, err = f.svc.Complete(f.ctx, &pb.Completion{Id: queuedForm, Lease: delivery.Lease, Outcome: "sent", TelegramMessageId: 700})
	f.must(err)
	f.exec("ALTER TABLE outbound_messages ADD CONSTRAINT fail_capacity_notice CHECK(body->>'code' IS DISTINCT FROM 'agreement_capacity_full') NOT VALID")
	failedUpdate := &pb.Update{Id: f.seq.Add(1), Actor: 54, Chat: 54, ChatType: "private", Callback: fmt.Sprintf("qa_%d_%d", q, positive), MessageId: 700, CallbackMessageEditable: true}
	if _, err := f.svc.Accept(f.ctx, failedUpdate); err == nil || f.answer(q, 54) != 0 {
		t.Fatal("failed warning partially saved a late answer")
	}
	f.exec("ALTER TABLE outbound_messages DROP CONSTRAINT fail_capacity_notice")
	_, err = f.svc.Accept(f.ctx, failedUpdate)
	f.must(err)
	if f.answer(q, 54) != positive {
		t.Fatal("late answer could not recover after outbox failure")
	}

	// Late Yes from an early registrant is stored once, warned and excluded.
	update := &pb.Update{Id: f.seq.Add(1), Actor: 50, Chat: 50, ChatType: "private", Callback: fmt.Sprintf("qa_%d_%d", q, positive), MessageId: 700, CallbackMessageEditable: true}
	receipt, err := f.svc.Accept(f.ctx, update)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, update)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(receipt.DeliveryIds, replay.DeliveryIds) || f.answer(q, 50) != positive {
		t.Fatal("late answer/replay was not preserved")
	}
	f.send(50, "", fmt.Sprintf("qa_%d_%d", q, negative))
	var warnings, removals int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FILTER(WHERE body->>'code'='agreement_capacity_full'),count(*) FILTER(WHERE kind='survey_remove_keyboard') FROM outbound_messages WHERE chat=50").Scan(&warnings, &removals))
	if warnings != 1 || removals != 2 || f.answer(q, 50) != positive {
		t.Fatal("late Yes must warn once, remove buttons and retain its first answer")
	}
	_, noView := f.send(51, "", fmt.Sprintf("qa_%d_%d", q, negative))
	if f.answer(q, 51) != negative || noView.Code == "agreement_capacity_full" {
		t.Fatal("late No must retain ordinary survey behavior")
	}

	// A previously displayed form button still works after hiding the question.
	var version int64
	f.must(f.db.QueryRow(f.ctx, "SELECT version FROM users WHERE telegram_id=52").Scan(&version))
	f.send(52, "", fmt.Sprintf("c:%d:sq_%d_%d", version, q, positive))
	if f.answer(q, 52) != positive {
		t.Fatal("old form answer was discarded")
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FILTER(WHERE body->>'code'='registration_completed'),count(*) FILTER(WHERE kind='survey_remove_keyboard') FROM outbound_messages WHERE chat=52").Scan(&warnings, &removals))
	if warnings != 1 || removals != 1 {
		t.Fatal("old form must get the completion warning and lose its buttons")
	}

	// New registration ends after personal fields, without asking agreement.
	f.seeded(60, "expectations")
	_, next := f.send(60, "Synthetic last field", "")
	if next.Kind != "confirm" || f.answer(q, 60) != 0 {
		t.Fatal("hidden agreement still blocks new registration", next)
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE chat=60 AND body->>'code'='registration_completed'").Scan(&warnings))
	if warnings != 1 {
		t.Fatal("new registration missed its completion notice")
	}
	f.send(9999, "", "final:no")
	if finalCount(f, "telegram_id=9909 AND status='invited'") != 1 || finalCount(f, "telegram_id IN (50,51,52,60)") != 0 {
		t.Fatal("late agreement overtook the existing reserve")
	}

	exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
	f.must(err)
	file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
	f.must(err)
	defer file.Close()
	rows, err := file.GetRows("Sheet1")
	f.must(err)
	surveyCol := len(rows[0]) - 1
	seen := 0
	for _, row := range rows[1:] {
		if row[0] == "50" || row[0] == "52" {
			seen++
			if row[surveyCol] != "Да <&>" {
				t.Fatal("late positive missing from export")
			}
		}
	}
	if seen != 2 {
		t.Fatal("late participants missing from export")
	}
}

func TestAdmissionCutoffUsesSaveTimeInsteadOfTransactionStart(t *testing.T) {
	f := newSurveyFixture(t, "admission_clock_test")
	q, positive := seedFinalConfirmation(f, 1)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	f.seeded(70, "registered")
	f.exec("UPDATE users SET registration_completed_at='2026-01-01' WHERE telegram_id=70")
	f.seeded(71, "expectations")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,71,$2)", q, positive)
	tx, err := f.db.Begin(f.ctx)
	f.must(err)
	defer tx.Rollback(f.ctx)
	var started time.Time
	f.must(tx.QueryRow(f.ctx, "SELECT now()").Scan(&started))
	closed, err := f.svc.CloseFinalAdmission(f.ctx, q)
	f.must(err)
	if !started.Before(closed.ClosedAt) {
		t.Fatal("transaction must predate the cutoff")
	}
	_, err = tx.Exec(f.ctx, store.Q("survey_answer"), q, int64(70), positive)
	f.must(err)
	var version int64
	f.must(tx.QueryRow(f.ctx, store.Q("save_user"), int64(71), "confirm", "Synthetic", "01.01.2000", "ИУ7-41", "79991234567", "Synthetic", "", "").Scan(&version))
	f.must(tx.Commit(f.ctx))
	var eligible int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM final_confirmation_eligible WHERE telegram_id IN (70,71)").Scan(&eligible))
	if eligible != 0 {
		t.Fatal("a transaction begun before closing bypassed the cutoff")
	}
}

func TestAdmissionMigrationPreservesActiveCampaign(t *testing.T) {
	f := newSurveyFixture(t, "admission_upgrade_test")
	q, positive := seedFinalConfirmation(f, 2)
	f.exec("UPDATE survey_questions SET in_registration=true WHERE id=$1", q)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	f.send(9999, "", "final:yes")
	dropAdmissionSchema(f)
	const snapshot = `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY telegram_id) FROM users u),
'questions',(SELECT jsonb_agg(to_jsonb(q)-'admission_closed_at' ORDER BY id) FROM survey_questions q),
'answers',(SELECT jsonb_agg(to_jsonb(a) ORDER BY question_id,telegram_id) FROM survey_answers a),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM outbound_messages o),
'invitations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY telegram_id) FROM final_confirmation_invitations i),
'campaign',(SELECT jsonb_agg(to_jsonb(c)) FROM final_confirmation_campaign c),
'history',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<=9)
)::text`
	var before, after string
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&before))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&after))
	if before != after {
		t.Fatal("migration changed existing answers, invitations or history")
	}
	var open bool
	f.must(f.db.QueryRow(f.ctx, "SELECT in_registration AND admission_closed_at IS NULL FROM survey_questions WHERE id=$1", q).Scan(&open))
	if !open {
		t.Fatal("deployment must not activate the admission cutoff")
	}
}
