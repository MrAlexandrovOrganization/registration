//go:build integration

package integration

import (
	"fmt"
	"slices"
	"testing"

	pb "registration.local/backend/api"
)

func finalInvitationJob(f *surveyFixture, actor int64) int64 {
	f.t.Helper()
	var job int64
	f.must(f.db.QueryRow(f.ctx, "SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=$1", actor).Scan(&job))
	return job
}

func pendingBroadcast(f *surveyFixture) *pb.View {
	f.t.Helper()
	f.send(1, "/broadcast final_pending", "")
	_, preview := f.send(1, "Synthetic reminder <&>", "")
	if preview.Kind != "broadcast_preview" || preview.Field != "final_pending" || len(preview.Numbers) != 2 {
		f.t.Fatal("missing final-pending preview", preview)
	}
	return preview
}

func TestFinalPendingSelectionAndDeliveryRecheck(t *testing.T) {
	f := newSurveyFixture(t, "final_pending_test")
	q, positive := seedFinalConfirmation(f, 14)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	for actor := int64(9999); actor >= 9991; actor-- {
		f.complete(f.claim(finalInvitationJob(f, actor)))
	}
	f.complete(f.claim(finalInvitationJob(f, 9986)))
	f.send(9999, "", "final:yes")
	f.send(9998, "", "final:no")
	f.exec("UPDATE users SET is_blocked=1 WHERE telegram_id=9997")
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id=9996")
	f.exec("UPDATE users SET is_counselor=1 WHERE telegram_id=9995")
	f.exec("UPDATE users SET has_registered=false WHERE telegram_id=9994")
	// Editing a finished form keeps eligibility; completion, not state, matters.
	f.exec("UPDATE users SET state='edit_name' WHERE telegram_id=9991")
	f.exec("UPDATE outbound_messages SET status='retry_wait' WHERE id=$1", finalInvitationJob(f, 9989))
	f.exec("UPDATE outbound_messages SET status='failed',error_code='transient' WHERE id=$1", finalInvitationJob(f, 9988))
	f.exec("UPDATE outbound_messages SET status='sent',telegram_message_id=NULL WHERE id=$1", finalInvitationJob(f, 9987))
	f.seeded(42, "registered")
	f.exec("UPDATE users SET registration_completed_at='2026-10-01' WHERE telegram_id=42")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,42,$2)", q, positive)
	preview := pendingBroadcast(f)
	if preview.Numbers[1] != 4 {
		t.Fatal("preview must include only delivered, unanswered, eligible invitations", preview)
	}
	broadcast := preview.Numbers[0]
	var jobs int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&jobs))
	if jobs != 0 {
		t.Fatal("preview started the reminder broadcast")
	}
	// A response between preview and start must not enter the snapshot.
	f.send(9993, "", "final:yes")
	start := &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", Text: fmt.Sprintf("/send %d", broadcast), MessageId: 700}
	r, err := f.svc.Accept(f.ctx, start)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, start)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(r.DeliveryIds, replay.DeliveryIds) {
		t.Fatal("duplicate start changed its receipts")
	}
	var recipients string
	f.must(f.db.QueryRow(f.ctx, "SELECT string_agg(chat::text,',' ORDER BY chat) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&recipients))
	if recipients != "9986,9991,9992" {
		t.Fatal("wrong reminder snapshot", recipients)
	}
	job := func(actor int64) int64 {
		t.Helper()
		var id int64
		f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=$1 AND chat=$2", broadcast, actor).Scan(&id))
		return id
	}
	checkCancelled := func(id int64) {
		t.Helper()
		d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: id, Worker: "survey-fixture-worker"})
		f.must(err)
		var state string
		f.must(f.db.QueryRow(f.ctx, "SELECT status FROM outbound_messages WHERE id=$1", id).Scan(&state))
		if d.Id != 0 || d.Kind != "" || state != "cancelled" {
			t.Fatal("answered participant received a reminder payload", d, state)
		}
	}
	f.send(1, fmt.Sprintf("/pause %d", broadcast), "")
	f.send(9992, "", "final:no")
	f.send(1, fmt.Sprintf("/resume %d", broadcast), "")
	checkCancelled(job(9992))
	// A newly delivered invitation does not expand an existing broadcast.
	f.complete(f.claim(finalInvitationJob(f, 9990)))
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&jobs))
	if jobs != 3 {
		t.Fatal("running reminder audience expanded")
	}
	// Exhausted/uncertain delivery, then an answer before manual retry.
	f.exec("UPDATE outbound_messages SET attempts=7 WHERE id=$1", job(9991))
	d := f.claim(job(9991))
	if d.Kind != "copy" || d.SourceChat != 1 || d.SourceMessage != 700 {
		t.Fatal("reminder lost the reviewed source", d)
	}
	_, err = f.svc.Complete(f.ctx, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: "uncertain"})
	f.must(err)
	f.send(9991, "", "final:yes")
	f.send(1, fmt.Sprintf("/retry %d", broadcast), "")
	checkCancelled(job(9991))
	checkCancelled(job(9992))
	f.complete(f.claim(job(9986)))
	if finalCount(f, "telegram_id=9986 AND status='invited'") != 1 {
		t.Fatal("reminder changed the final answer or invitation")
	}
	// The new guard must not change unrelated broadcast audiences.
	f.send(1, "/broadcast registered", "")
	_, ordinary := f.send(1, "Synthetic ordinary broadcast", "")
	f.send(1, fmt.Sprintf("/send %d", ordinary.Numbers[0]), "")
	var ordinaryJob int64
	f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=$1 AND chat=9993", ordinary.Numbers[0]).Scan(&ordinaryJob))
	f.complete(f.claim(ordinaryJob))
}

func TestFinalPendingRechecksChangedEligibility(t *testing.T) {
	for _, test := range []struct{ name, change string }{
		{"blocked", "UPDATE users SET is_blocked=1 WHERE telegram_id=9999"},
		{"staff", "UPDATE users SET is_staff=1 WHERE telegram_id=9999"},
		{"counselor", "UPDATE users SET is_counselor=1 WHERE telegram_id=9999"},
		{"incomplete", "UPDATE users SET has_registered=false WHERE telegram_id=9999"},
		{"excluded", "UPDATE final_confirmation_invitations SET status='excluded' WHERE telegram_id=9999"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSurveyFixture(t, "pending_guard_"+test.name)
			q, positive := seedFinalConfirmation(f, 1)
			f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
			f.complete(f.claim(finalInvitationJob(f, 9999)))
			preview := pendingBroadcast(f)
			if preview.Numbers[1] != 1 {
				t.Fatal("eligible invitation missing")
			}
			f.send(1, fmt.Sprintf("/send %d", preview.Numbers[0]), "")
			f.exec(test.change)
			var job int64
			f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=$1", preview.Numbers[0]).Scan(&job))
			d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "survey-fixture-worker"})
			f.must(err)
			if d.Id != 0 {
				t.Fatal("ineligible participant received a reminder", d)
			}
		})
	}
}

func TestFinalPendingWithoutCampaign(t *testing.T) {
	f := newSurveyFixture(t, "pending_no_campaign")
	f.seeded(42, "registered")
	f.send(42, "/broadcast final_pending", "")
	var broadcasts int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM broadcasts").Scan(&broadcasts))
	if broadcasts != 0 {
		t.Fatal("participant could prepare an administrative broadcast")
	}
	preview := pendingBroadcast(f)
	if preview.Numbers[1] != 0 {
		t.Fatal("no campaign must produce an empty audience")
	}
}
