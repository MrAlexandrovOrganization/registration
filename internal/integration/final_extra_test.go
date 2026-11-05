//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xuri/excelize/v2"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/sheets"
	"registration.local/backend/internal/store"
)

func dropFinalExtraSchema(f *surveyFixture) {
	f.t.Helper()
	f.exec(`DROP VIEW final_confirmation_invitation_eligible;
ALTER TABLE final_confirmation_invitations DROP COLUMN is_manual;
ALTER TABLE final_confirmation_invitations DROP CONSTRAINT final_confirmation_invitations_status_check;
ALTER TABLE final_confirmation_invitations ADD CONSTRAINT final_confirmation_invitations_status_check
 CHECK(status IN ('invited','yes','no','excluded'));
DELETE FROM schema_migrations WHERE version>=11`)
}

func prepareFinalExtra(f *surveyFixture, command string) *pb.View {
	f.t.Helper()
	before := finalCount(f, "true")
	r, v := f.send(1, command, "")
	if v.Kind != "final_invites_preview" || len(v.Numbers) == 0 || len(r.DeliveryIds) != 2 || finalCount(f, "true") != before {
		f.t.Fatal("preview sent invitations or did not show its recipients", v)
	}
	deliverFinalPreview(f, r.DeliveryIds[len(r.DeliveryIds)-1], 700)
	return v
}

func TestFinalExtraFirstNinetyPositiveAnswers(t *testing.T) {
	f := newSurveyFixture(t, "final_extra_capacity")
	q, positive := seedFinalConfirmation(f, 130)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	preview := prepareFinalExtra(f, "/final_more 40")
	if len(preview.Numbers) != 40 {
		t.Fatal("wrong extra audience")
	}
	confirm := &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", MessageId: 700, Callback: preview.Buttons[0].Data, CallbackMessageEditable: true}
	r, err := f.svc.Accept(f.ctx, confirm)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, confirm)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(r.DeliveryIds, replay.DeliveryIds) || finalCount(f, "true") != 130 {
		t.Fatal("extra send/replay did not preserve its recipient set")
	}
	f.send(1, "", preview.Buttons[0].Data)
	if finalCount(f, "true") != 130 {
		t.Fatal("consumed preview sent another batch")
	}
	// Simultaneous clicks contend on the same allocation lock. Processing order,
	// not registration rank, decides the last available seats.
	var wg sync.WaitGroup
	failures := make(chan error, 130)
	for actor := int64(9999); actor >= 9870; actor-- {
		update := &pb.Update{Id: f.seq.Add(1), Actor: actor, Chat: actor, ChatType: "private", MessageId: 700, Callback: "final:yes", CallbackMessageEditable: true}
		wg.Go(func() { _, err := f.svc.Accept(f.ctx, update); failures <- err })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		f.must(err)
	}
	if finalCount(f, "status='yes'") != 90 || finalCount(f, "status='late'") != 40 {
		t.Fatal("positive answer race overbooked or lost late answers")
	}
	var acceptedReplies, lateReplies int
	f.must(f.db.QueryRow(f.ctx, `SELECT count(*) FILTER(WHERE body->>'code'='final_yes_saved'),
count(*) FILTER(WHERE body->>'code'='final_late_saved') FROM outbound_messages`).Scan(&acceptedReplies, &lateReplies))
	if acceptedReplies != 90 || lateReplies != 40 {
		t.Fatal("late participants received a successful confirmation")
	}
	var late int64
	f.must(f.db.QueryRow(f.ctx, "SELECT telegram_id FROM final_confirmation_invitations WHERE status='late' LIMIT 1").Scan(&late))
	f.send(late, "", "final:no")
	f.send(late, "", "final:yes")
	_, already := f.send(1, fmt.Sprintf("/final_invite %d", late), "")
	if finalCount(f, "status='late'") != 40 || already.Code != "final_extra_already_invited" {
		t.Fatal("an old button or personal invitation changed a final late answer")
	}
	exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
	f.must(err)
	file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
	f.must(err)
	defer file.Close()
	rows, err := file.GetRows("Sheet1")
	f.must(err)
	col := slices.Index(rows[0], "Финальное подтверждение участия")
	recorder := &sheetRecorder{}
	worker := &sheets.Worker{Service: f.svc, Writer: recorder}
	written, err := worker.Sync(f.ctx)
	f.must(err)
	if !written || col < 0 || len(rows) != len(recorder.table.Rows) {
		t.Fatal("late answers missing from synchronized export")
	}
	lateRows := 0
	for i, row := range recorder.table.Rows[1:] {
		if strings.HasSuffix(row[col], " — мест нет") {
			lateRows++
			if rows[i+1][col] != row[col] {
				t.Fatal("XLSX and Sheets disagree on a late positive answer")
			}
		}
	}
	if lateRows != 40 {
		t.Fatal("export does not distinguish late positives from accepted seats")
	}
	// A personal invitation has no special seat beyond the same 90 winners.
	f.seeded(500, "registered")
	personal := prepareFinalExtra(f, "/final_invite 500")
	f.send(1, "", personal.Buttons[0].Data)
	f.send(500, "", "final:yes")
	if finalCount(f, "telegram_id=500 AND status='late'") != 1 || finalCount(f, "status='yes'") != 90 {
		t.Fatal("personal invitation bypassed the positive-answer capacity")
	}
}

func TestFinalPersonalInvitationPreviewAndEligibility(t *testing.T) {
	f := newSurveyFixture(t, "final_personal_invite")
	q, positive := seedFinalConfirmation(f, 91)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	_, err := f.svc.CloseFinalAdmission(f.ctx, q)
	f.must(err)
	f.seeded(500, "registered")
	f.exec("UPDATE users SET username='@Special_One' WHERE telegram_id=500")
	r, preview := f.send(1, "/final_invite @special_one", "")
	if preview.Kind != "final_invites_preview" || preview.Field != "manual" || len(r.DeliveryIds) != 2 || !slices.Equal(preview.Numbers, []int64{500}) {
		t.Fatal("personal preview did not resolve the stored username", preview)
	}
	_, stale := f.send(1, "", preview.Buttons[0].Data)
	if stale.Code != "final_extra_stale" || finalCount(f, "telegram_id=500") != 0 {
		t.Fatal("undelivered preview authorized an invitation")
	}
	deliverFinalPreview(f, r.DeliveryIds[len(r.DeliveryIds)-1], 700)
	f.seeded(2, "new")
	f.exec("INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES(2,'admin',1),(2,'message_sender',1)")
	f.send(2, "", preview.Buttons[0].Data)
	if finalCount(f, "telegram_id=500") != 0 {
		t.Fatal("another operator consumed a personal preview")
	}
	// Failure rolls back consumption, the invitation and its delivery together.
	f.exec("ALTER TABLE outbound_messages ADD CONSTRAINT fail_personal CHECK(body->>'code' IS DISTINCT FROM 'final_invitation') NOT VALID")
	confirm := &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", MessageId: 700, Callback: preview.Buttons[0].Data, CallbackMessageEditable: true}
	if _, err = f.svc.Accept(f.ctx, confirm); err == nil || finalCount(f, "telegram_id=500") != 0 {
		t.Fatal("failed personal send partially committed")
	}
	f.exec("ALTER TABLE outbound_messages DROP CONSTRAINT fail_personal")
	_, err = f.svc.Accept(f.ctx, confirm)
	f.must(err)
	if finalCount(f, "telegram_id=500 AND is_manual AND status='invited'") != 1 || f.answer(q, 500) != 0 {
		t.Fatal("personal exception changed agreement or failed to invite")
	}
	job := finalInvitationJob(f, 500)
	f.complete(f.claim(job))
	reminder := pendingBroadcast(f)
	if reminder.Numbers[1] != 1 {
		t.Fatal("personal invitation missing from final_pending")
	}
	f.send(500, "", "final:yes")
	if finalCount(f, "telegram_id=500 AND status='yes'") != 1 {
		t.Fatal("personal invitation incorrectly required agreement or queue order")
	}
	var oldBroadcast int64
	f.must(f.db.QueryRow(f.ctx, store.Q("broadcast_create"), int64(1), "registered", "view", int64(0), int64(0)).Scan(&oldBroadcast))
	f.exec("UPDATE survey_questions SET broadcast_id=$2 WHERE id=$1", q, oldBroadcast)
	targetAdmissionSurvey(f, q, 500)
	_, oldAgreement := f.send(500, "", fmt.Sprintf("qa_%d_%d", q, positive))
	if oldAgreement.Code == "agreement_capacity_full" || finalCount(f, "telegram_id=500 AND status='yes'") != 1 {
		t.Fatal("an old first-agreement message contradicted the accepted personal seat")
	}
	for i, change := range []string{"has_registered=false", "is_blocked=1", "is_staff=1", "is_counselor=1"} {
		actor := int64(510 + i)
		f.seeded(actor, "registered")
		f.exec("UPDATE users SET "+change+" WHERE telegram_id=$1", actor)
		_, v := f.send(1, fmt.Sprintf("/final_invite %d", actor), "")
		if v.Code != "final_extra_ineligible" || finalCount(f, fmt.Sprintf("telegram_id=%d", actor)) != 0 {
			t.Fatal("personal invitation bypassed a required user condition", v)
		}
	}
	// Queue preview cancellation and rechecking do not silently replace targets.
	cancel := prepareFinalExtra(f, "/final_more 1")
	f.send(1, "", cancel.Buttons[1].Data)
	if finalCount(f, "true") != 91 {
		t.Fatal("cancelled extra preview sent a message")
	}
	changed := prepareFinalExtra(f, "/final_more 1")
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id=$1", changed.Numbers[0])
	f.send(1, "", changed.Buttons[0].Data)
	if finalCount(f, "true") != 91 {
		t.Fatal("ineligible preview recipient was invited")
	}
	f.seeded(600, "registered")
	r, operatorPreview := f.send(2, "/final_invite 600", "")
	if operatorPreview.Kind != "final_invites_preview" {
		t.Fatal("authorized operator missed the preview")
	}
	rows, err := f.db.Query(f.ctx, "SELECT id FROM outbound_messages WHERE chat=2 AND status='pending' AND id<=$1 ORDER BY id", r.DeliveryIds[len(r.DeliveryIds)-1])
	f.must(err)
	var deliveries []int64
	for rows.Next() {
		var id int64
		f.must(rows.Scan(&id))
		deliveries = append(deliveries, id)
	}
	f.must(rows.Err())
	rows.Close()
	for _, id := range deliveries {
		f.complete(f.claim(id))
	}
	f.exec("DELETE FROM user_permissions WHERE telegram_id=2 AND permission='admin'")
	f.send(2, "", operatorPreview.Buttons[0].Data)
	if finalCount(f, "telegram_id=600") != 0 {
		t.Fatal("revoked operator confirmed a personal invitation")
	}
	f.seeded(601, "registered")
	f.exec("UPDATE users SET state=$2 WHERE telegram_id=$1", int64(601), fmt.Sprintf("survey_%d", q))
	oldForm := prepareFinalExtra(f, "/final_invite 601")
	f.send(1, "", oldForm.Buttons[0].Data)
	f.send(601, "", "final:yes")
	var version int64
	f.must(f.db.QueryRow(f.ctx, "SELECT version FROM users WHERE telegram_id=601").Scan(&version))
	f.send(601, "", fmt.Sprintf("c:%d:sq_%d_%d", version, q, positive))
	var waitingNotices int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE chat=601 AND body->>'code'='registration_completed'").Scan(&waitingNotices))
	if waitingNotices != 0 || finalCount(f, "telegram_id=601 AND status='yes'") != 1 {
		t.Fatal("finishing an old form contradicted an already accepted seat")
	}
}

func TestFinalExtraMigrationPreservesExistingCampaign(t *testing.T) {
	f := newSurveyFixture(t, "final_extra_upgrade")
	q, positive := seedFinalConfirmation(f, 2)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	f.send(9999, "", "final:yes")
	dropFinalExtraSchema(f)
	const snapshot = `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY telegram_id) FROM users u),
'answers',(SELECT jsonb_agg(to_jsonb(a) ORDER BY question_id,telegram_id) FROM survey_answers a),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM outbound_messages o),
'invitations',(SELECT jsonb_agg(to_jsonb(i)-'is_manual' ORDER BY telegram_id) FROM final_confirmation_invitations i),
'campaign',(SELECT jsonb_agg(to_jsonb(c)) FROM final_confirmation_campaign c),
'history',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<=10)
)::text`
	var before, after string
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&before))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&after))
	if before != after || finalCount(f, "is_manual") != 0 {
		t.Fatal("migration changed old seats, flags, answers or pending deliveries")
	}
}
