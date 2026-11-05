//go:build integration

package integration

import (
	"fmt"
	"slices"
	"testing"

	pb "registration.local/backend/api"
)

func deliverFinalInvitation(f *surveyFixture, actor int64) {
	f.t.Helper()
	var id int64
	f.must(f.db.QueryRow(f.ctx, "SELECT delivery_id FROM final_confirmation_invitations WHERE telegram_id=$1", actor).Scan(&id))
	f.complete(f.claim(id))
}

func declinePreview(f *surveyFixture, target, message int64) *pb.Update {
	f.t.Helper()
	r, preview := f.send(1, fmt.Sprintf("/final_decline %d", target), "")
	if preview.Kind != "final_decline_preview" || len(preview.Buttons) != 2 {
		f.t.Fatal("missing decline preview", preview)
	}
	deliverFinalPreview(f, r.DeliveryIds[len(r.DeliveryIds)-1], message)
	return &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", MessageId: message, Callback: preview.Buttons[0].Data, CallbackMessageEditable: true}
}

func TestOrganizerDeclineConfirmedParticipantAndAtomicReplacement(t *testing.T) {
	f := newSurveyFixture(t, "final_admin_decline_test")
	q, positive := seedFinalConfirmation(f, 93)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	deliverFinalInvitation(f, 9999)
	f.send(9999, "", "final:yes")
	confirmation := declinePreview(f, 9999, 900)
	if finalCount(f, "status='yes'") != 1 || finalCount(f, "true") != 90 {
		t.Fatal("preview changed participation")
	}
	// The confirmation cannot be applied from a different message.
	forged := *confirmation
	forged.Id = f.seq.Add(1)
	forged.MessageId = 901
	_, err := f.svc.Accept(f.ctx, &forged)
	f.must(err)
	if finalCount(f, "status='yes'") != 1 {
		t.Fatal("confirmation without its delivered preview changed participation")
	}
	f.exec("ALTER TABLE outbound_messages ADD CONSTRAINT fail_admin_replacement CHECK(body->>'code' IS DISTINCT FROM 'final_invitation') NOT VALID")
	if _, err = f.svc.Accept(f.ctx, confirmation); err == nil || finalCount(f, "status='yes'") != 1 || finalCount(f, "true") != 90 {
		t.Fatal("failed replacement partially committed an organizer decline")
	}
	f.exec("ALTER TABLE outbound_messages DROP CONSTRAINT fail_admin_replacement")
	receipt, err := f.svc.Accept(f.ctx, confirmation)
	f.must(err)
	replay, err := f.svc.Accept(f.ctx, confirmation)
	f.must(err)
	if !replay.Duplicate || !slices.Equal(receipt.DeliveryIds, replay.DeliveryIds) ||
		finalCount(f, "telegram_id=9999 AND status='no'") != 1 || finalCount(f, "telegram_id=9909 AND status='invited'") != 1 ||
		finalCount(f, "status IN ('yes','invited')") != 90 || f.answer(q, 9999) != positive {
		t.Fatal("organizer decline/replacement/replay changed the wrong data")
	}
	confirmation.Id = f.seq.Add(1)
	_, err = f.svc.Accept(f.ctx, confirmation)
	f.must(err)
	f.send(9999, "", "final:yes")
	var audits int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM audit_events WHERE action='final_admin_decline_yes' AND target=9999").Scan(&audits))
	if finalCount(f, "true") != 91 || audits != 1 || finalCount(f, "telegram_id=9999 AND status='no'") != 1 {
		t.Fatal("repeat confirmation or an old participant button reclaimed the place")
	}
}

func TestOrganizerDeclineStaleStateAndPermissions(t *testing.T) {
	f := newSurveyFixture(t, "final_admin_decline_access_test")
	q, positive := seedFinalConfirmation(f, 92)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	f.seeded(2, "registered")
	for _, permission := range []string{"message_sender", "admin", "table_viewer"} {
		f.exec("DELETE FROM user_permissions WHERE telegram_id=2")
		f.exec("INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES(2,$1,1)", permission)
		r, _ := f.send(2, "/final_decline 9999", "")
		if len(r.DeliveryIds) != 0 {
			t.Fatal("a single role exposed the organizer decline interface")
		}
	}
	confirmation := declinePreview(f, 9999, 910)
	f.send(9999, "", "final:yes")
	_, err := f.svc.Accept(f.ctx, confirmation)
	f.must(err)
	if finalCount(f, "telegram_id=9999 AND status='yes'") != 1 || finalCount(f, "true") != 90 {
		t.Fatal("an old invited-state preview overrode a new Yes answer")
	}
	// An unanswered invitation can also be declined without creating a second campaign.
	confirmation = declinePreview(f, 9998, 911)
	_, err = f.svc.Accept(f.ctx, confirmation)
	f.must(err)
	if finalCount(f, "telegram_id=9998 AND status='no'") != 1 || finalCount(f, "true") != 91 {
		t.Fatal("organizer decline did not release an unanswered place")
	}
	_, notActive := f.send(1, "/final_decline 42", "")
	if notActive.Code != "final_decline_not_active" {
		t.Fatal("unknown participants must not be enrolled/declined by this command")
	}
	r, preview := f.send(1, "/final_decline 9997", "")
	deliverFinalPreview(f, r.DeliveryIds[len(r.DeliveryIds)-1], 912)
	_, err = f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", MessageId: 912, Callback: preview.Buttons[1].Data, CallbackMessageEditable: true})
	f.must(err)
	if finalCount(f, "telegram_id=9997 AND status='invited'") != 1 || finalCount(f, "true") != 91 {
		t.Fatal("cancel changed participation")
	}
	_, err = f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 1, Chat: 1, ChatType: "private", MessageId: 912, Callback: preview.Buttons[0].Data, CallbackMessageEditable: true})
	f.must(err)
	if finalCount(f, "telegram_id=9997 AND status='invited'") != 1 || finalCount(f, "true") != 91 {
		t.Fatal("cancelled preview could still confirm a decline")
	}
	f.exec("INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES(4,'admin',1),(4,'message_sender',1)")
	r, preview = f.send(4, "/final_decline 9997", "")
	job := r.DeliveryIds[len(r.DeliveryIds)-1]
	f.complete(f.claim(job))
	f.exec("DELETE FROM user_permissions WHERE telegram_id=4 AND permission='admin'")
	_, err = f.svc.Accept(f.ctx, &pb.Update{Id: f.seq.Add(1), Actor: 4, Chat: 4, ChatType: "private", MessageId: 700, Callback: preview.Buttons[0].Data, CallbackMessageEditable: true})
	f.must(err)
	if finalCount(f, "telegram_id=9997 AND status='invited'") != 1 {
		t.Fatal("revoked operator permission still authorized a decline")
	}
	f.exec("INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES(4,'admin',1)")
	r, _ = f.send(4, "/final_decline 9997", "")
	job = r.DeliveryIds[len(r.DeliveryIds)-1]
	f.exec("DELETE FROM user_permissions WHERE telegram_id=4 AND permission='admin'")
	delivery, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "survey-fixture-worker"})
	f.must(err)
	if delivery.Id != 0 {
		t.Fatal("a queued preview leaked after operator access was revoked")
	}
}

func TestOrganizerDeclineByUsername(t *testing.T) {
	f := newSurveyFixture(t, "final_decline_username_test")
	q, positive := seedFinalConfirmation(f, 92)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	f.exec("UPDATE users SET username='Synthetic_Nick' WHERE telegram_id=9999")
	for _, input := range []string{"@synthetic_nick", "SYNTHETIC_NICK"} {
		_, preview := f.send(1, "/final_decline "+input, "")
		if preview.Kind != "final_decline_preview" || len(preview.Buttons) != 2 || preview.Buttons[0].Data != "final_decline:9999:invited" {
			t.Fatal("username failed to resolve to the correct confirmation", preview)
		}
	}
	_, missing := f.send(1, "/final_decline @missing_nick", "")
	if missing.Code != "final_decline_id_required" {
		t.Fatal("unknown username must ask for an ID")
	}
	f.exec("UPDATE users SET username='synthetic_nick' WHERE telegram_id=9998")
	_, ambiguous := f.send(1, "/final_decline @synthetic_nick", "")
	if ambiguous.Code != "final_decline_id_required" || finalCount(f, "status='no'") != 0 {
		t.Fatal("duplicate/stale usernames selected a participant")
	}
}
