//go:build integration

package integration

import (
	"fmt"
	"slices"
	"testing"

	pb "registration.local/backend/api"
)

func confirmedBroadcast(f *surveyFixture) *pb.View {
	f.t.Helper()
	f.send(1, "/broadcast final_confirmed", "")
	_, preview := f.send(1, "Synthetic participant information <&>", "")
	if preview.Kind != "broadcast_preview" || preview.Field != "final_confirmed" || len(preview.Numbers) != 2 {
		f.t.Fatal("missing confirmed-participant preview", preview)
	}
	return preview
}

func TestFinalConfirmedAudienceAndDeliveryRecheck(t *testing.T) {
	f := newSurveyFixture(t, "final_confirmed_broadcast")
	q, positive := seedFinalConfirmation(f, 10)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	for actor := int64(9999); actor >= 9990; actor-- {
		f.complete(f.claim(finalInvitationJob(f, actor)))
		if actor >= 9992 {
			f.send(actor, "", "final:yes")
		}
	}
	f.send(9991, "", "final:no")
	// The four required flags apply independently of a previously accepted Yes.
	f.exec("UPDATE users SET is_blocked=1 WHERE telegram_id=9997")
	f.exec("UPDATE users SET is_staff=1 WHERE telegram_id=9996")
	f.exec("UPDATE users SET is_counselor=1 WHERE telegram_id=9995")
	f.exec("UPDATE users SET has_registered=false WHERE telegram_id=9994")
	f.exec("UPDATE users SET state='edit_name' WHERE telegram_id=9999")
	// A personal invitation can have a confirmed seat without a first agreement.
	f.seeded(500, "registered")
	f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status,is_manual) VALUES(500,'yes',true)")
	for offset, state := range []string{"late", "excluded"} {
		actor := int64(501 + offset)
		f.seeded(actor, "registered")
		f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status) VALUES($1,$2)", actor, state)
	}
	f.seeded(42, "registered")
	f.exec("UPDATE users SET registration_completed_at='2026-10-02' WHERE telegram_id=42")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,42,$2)", q, positive)
	preview := confirmedBroadcast(f)
	if preview.Numbers[1] != 5 {
		t.Fatal("only accepted final confirmations may enter the audience", preview)
	}
	broadcast := preview.Numbers[0]
	var jobs int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&jobs))
	if jobs != 0 {
		t.Fatal("preview started participant delivery")
	}
	// A recorded decline after preview must not enter the /send snapshot.
	f.exec("UPDATE final_confirmation_invitations SET status='no' WHERE telegram_id=9998")
	start := &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", Text: fmt.Sprintf("/send %d", broadcast), MessageId: 700}
	r, err := f.svc.Accept(f.ctx, start)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, start)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(r.DeliveryIds, replay.DeliveryIds) {
		t.Fatal("duplicate /send changed its receipts")
	}
	var recipients string
	f.must(f.db.QueryRow(f.ctx, "SELECT string_agg(chat::text,',' ORDER BY chat) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&recipients))
	if recipients != "500,9992,9993,9999" {
		t.Fatal("incorrect confirmed audience snapshot", recipients)
	}
	// Later confirmations cannot expand an already started broadcast.
	f.send(9990, "", "final:yes")
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", broadcast).Scan(&jobs))
	if jobs != 4 {
		t.Fatal("new confirmation expanded the audience")
	}
	f.send(1, fmt.Sprintf("/pause %d", broadcast), "")
	f.exec("UPDATE final_confirmation_invitations SET status='no' WHERE telegram_id=9993")
	f.send(1, fmt.Sprintf("/resume %d", broadcast), "")
	f.exec("UPDATE outbound_messages SET status='failed',error_code='uncertain' WHERE broadcast_id=$1 AND chat=9992", broadcast)
	f.exec("UPDATE users SET is_counselor=1 WHERE telegram_id=9992")
	f.send(1, fmt.Sprintf("/retry %d", broadcast), "")
	for _, actor := range []int64{500, 9992, 9993, 9999} {
		var job int64
		f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=$1 AND chat=$2", broadcast, actor).Scan(&job))
		d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "survey-fixture-worker"})
		f.must(err)
		if actor == 500 || actor == 9999 {
			if d.Id != job || d.Kind != "copy" || d.SourceChat != 1 || d.SourceMessage != 700 {
				t.Fatal("confirmed participant lost the reviewed message", d)
			}
			f.complete(d)
		} else {
			var state string
			f.must(f.db.QueryRow(f.ctx, "SELECT status FROM outbound_messages WHERE id=$1", job).Scan(&state))
			if d.Id != 0 || d.Kind != "" || state != "cancelled" {
				t.Fatal("former participant received a broadcast payload", d, state)
			}
		}
	}
}

func TestFinalConfirmedAudienceWithoutCampaign(t *testing.T) {
	f := newSurveyFixture(t, "confirmed_no_campaign")
	f.seeded(42, "registered")
	f.send(42, "/broadcast final_confirmed", "")
	var broadcasts int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM broadcasts").Scan(&broadcasts))
	if broadcasts != 0 {
		t.Fatal("participant could prepare an operator broadcast")
	}
	if preview := confirmedBroadcast(f); preview.Numbers[1] != 0 {
		t.Fatal("missing campaign must yield an empty confirmed audience", preview)
	}
}
