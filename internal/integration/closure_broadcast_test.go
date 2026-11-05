//go:build integration

package integration

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func closurePreview(f *surveyFixture, audience, code string, count int64) *pb.View {
	f.t.Helper()
	r, preview := f.send(1, "/broadcast "+audience, "")
	if len(r.DeliveryIds) != 2 || preview.Kind != "broadcast_preview" || preview.Field != audience || len(preview.Numbers) != 2 || preview.Numbers[1] != count {
		f.t.Fatal("missing closure preview", audience, r, preview)
	}
	var kind, gotCode string
	f.must(f.db.QueryRow(f.ctx, "SELECT kind,body->>'code' FROM outbound_messages WHERE id=$1", r.DeliveryIds[0]).Scan(&kind, &gotCode))
	if kind != "view" || gotCode != code {
		f.t.Fatal("preview has a different message", kind, gotCode)
	}
	var jobs int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", preview.Numbers[0]).Scan(&jobs))
	if jobs != 0 {
		f.t.Fatal("preparing a closure message started delivery")
	}
	return preview
}

func TestClosureBroadcastAudiencesAndRecheck(t *testing.T) {
	f := newSurveyFixture(t, "closure_audiences")
	q, positive := seedFinalConfirmation(f, 4)
	f.send(1, fmt.Sprintf("/final_start %d", prepareFinalConfirmation(f, q, positive)), "")
	for _, actor := range []int64{9999, 9998, 9997} {
		f.complete(f.claim(finalInvitationJob(f, actor)))
	}
	f.exec("UPDATE final_confirmation_invitations SET status='yes' WHERE telegram_id=9998")
	f.exec("UPDATE final_confirmation_invitations SET status='no' WHERE telegram_id=9997")
	// 9996's invitation is still pending: it belongs to none of the closure groups.
	var firstBroadcast int64
	f.must(f.db.QueryRow(f.ctx, "INSERT INTO broadcasts(actor,audience,kind,status) VALUES(1,'registered','survey','running') RETURNING id").Scan(&firstBroadcast))
	f.exec("UPDATE survey_questions SET broadcast_id=$2,admission_closed_at='2026-11-01' WHERE id=$1", q, firstBroadcast)
	f.exec("UPDATE survey_answers SET answered_at='2026-10-02' WHERE question_id=$1", q)
	firstDelivery := func(actor int64, form bool) {
		t.Helper()
		if form {
			f.exec(`INSERT INTO outbound_messages(chat,priority,kind,body,status,telegram_message_id)
VALUES($1,'interactive','view',jsonb_build_object('kind','survey_question','numbers',jsonb_build_array($2::bigint::text)),'sent',700)`, actor, q)
		} else {
			f.exec(`INSERT INTO outbound_messages(chat,priority,kind,body,broadcast_id,status,telegram_message_id)
VALUES($1,'broadcast','view',jsonb_build_object('kind','survey_broadcast','numbers',jsonb_build_array($2::bigint::text)),$3,'sent',700)`, actor, q, firstBroadcast)
		}
	}
	for group := int64(11); group <= 13; group++ {
		for offset := int64(0); offset <= 5; offset++ {
			actor := group*10 + offset
			f.seeded(actor, "registered")
			f.exec("UPDATE users SET registration_completed_at='2026-10-02' WHERE telegram_id=$1", actor)
			if group == 11 {
				f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id,answered_at) VALUES($1,$2,$3,'2026-10-02')", q, actor, positive)
			}
			if group == 12 {
				firstDelivery(actor, offset == 0) // Both in-form and broadcast delivery count.
			}
			if offset >= 2 {
				change := []string{"is_blocked=1", "is_staff=1", "is_counselor=1", "has_registered=false"}[offset-2]
				f.exec("UPDATE users SET "+change+" WHERE telegram_id=$1", actor)
			}
		}
	}
	// Late first Yes belongs to the fourth group, never the ordinary queue.
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id,answered_at) VALUES($1,131,$2,'2026-11-02')", q, positive)
	// A personal invite with no agreement must never receive a first-answer refusal.
	f.seeded(140, "registered")
	firstDelivery(140, false)
	f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status,is_manual) VALUES(140,'invited',true)")
	// First No, final No/Yes, excluded and late are all outside these audiences.
	f.seeded(150, "registered")
	f.exec("INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,150,$2)", q, f.options(q)[1])
	for i, state := range []string{"excluded", "late", "no", "yes"} {
		actor := int64(151 + i)
		f.seeded(actor, "registered")
		firstDelivery(actor, false)
		f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status) VALUES($1,$2)", actor, state)
	}
	// Profile editing does not remove someone who has already finished registration.
	f.exec("UPDATE users SET state='edit_name' WHERE telegram_id=110")
	tests := []struct {
		audience, code string
		members        []int64
	}{
		{"final_unanswered", "closure_waitlist", []int64{9999}},
		{"final_waiting", "closure_waitlist", []int64{110, 111}},
		{"agreement_unanswered", "closure_agreement_unanswered", []int64{120, 121}},
		{"registration_waiting", "closure_waitlist", []int64{130, 131}},
	}
	previews := make(map[string]*pb.View)
	for _, test := range tests {
		rows, err := f.db.Query(f.ctx, store.Q("special_broadcast_recipients"), test.audience, int64(0))
		f.must(err)
		members, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		f.must(err)
		if !slices.Equal(members, test.members) {
			t.Fatal("incorrect closure audience", test.audience, members)
		}
		previews[test.audience] = closurePreview(f, test.audience, test.code, int64(len(test.members)))
	}
	// An answer between preview and /send is excluded from the initial snapshot.
	f.exec("UPDATE final_confirmation_invitations SET status='yes' WHERE telegram_id=9999")
	for _, test := range tests {
		preview := previews[test.audience]
		f.send(1, fmt.Sprintf("/send %d", preview.Numbers[0]), "")
		// Repeated /send does not add recipients or create a second copy.
		f.send(1, fmt.Sprintf("/send %d", preview.Numbers[0]), "")
		var count int64
		f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", preview.Numbers[0]).Scan(&count))
		want := int64(len(test.members))
		if test.audience == "final_unanswered" {
			want = 0
		}
		if count != want {
			t.Fatal("incorrect /send snapshot", test.audience, count)
		}
	}
	// Changes after /send must also be checked, including after pause/resume/retry.
	f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status) VALUES(111,'invited'),(131,'invited')")
	f.send(121, "", fmt.Sprintf("qa_%d_%d", q, positive))
	if f.answer(q, 121) != positive {
		t.Fatal("legacy first button did not record a late answer")
	}
	// Processing that answer also fills the remaining ordinary invitation window:
	// 110 now has a second invitation and must skip its queued closure message too.
	for _, test := range tests[1:] {
		id := previews[test.audience].Numbers[0]
		f.send(1, fmt.Sprintf("/pause %d", id), "")
		f.send(1, fmt.Sprintf("/resume %d", id), "")
		f.exec("UPDATE outbound_messages SET status='failed',error_code='uncertain' WHERE broadcast_id=$1 AND chat=$2", id, test.members[1])
		f.send(1, fmt.Sprintf("/retry %d", id), "")
		for n, actor := range test.members {
			var job int64
			f.must(f.db.QueryRow(f.ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=$1 AND chat=$2", id, actor).Scan(&job))
			d, err := f.svc.Claim(f.ctx, &pb.ClaimRequest{Id: job, Worker: "survey-fixture-worker"})
			f.must(err)
			if n == 0 && test.audience != "final_waiting" {
				if d.Id != job || d.Kind != "view" || d.View.Code != test.code || len(d.View.Buttons) != 0 {
					t.Fatal("reviewed closure message was not delivered", d)
				}
				f.complete(d)
			} else {
				var state string
				f.must(f.db.QueryRow(f.ctx, "SELECT status FROM outbound_messages WHERE id=$1", job).Scan(&state))
				if d.Id != 0 || d.View != nil || state != "cancelled" {
					t.Fatal("stale closure message exposed a payload", d, state)
				}
			}
		}
	}
	// 121 changed groups after the snapshot; the fourth broadcast must not grow.
	var jobs int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=$1", previews["registration_waiting"].Numbers[0]).Scan(&jobs))
	if jobs != 2 {
		t.Fatal("started closure audience grew")
	}
}

func TestClosureBroadcastAccessAndNoCampaign(t *testing.T) {
	f := newSurveyFixture(t, "closure_access")
	f.seeded(42, "registered")
	for _, audience := range []string{"final_unanswered", "final_waiting", "agreement_unanswered", "registration_waiting"} {
		f.send(42, "/broadcast "+audience, "")
		var count int
		f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM broadcasts").Scan(&count))
		if count != 0 {
			t.Fatal("participant prepared a closure broadcast")
		}
	}
	for _, audience := range []string{"final_unanswered", "final_waiting", "agreement_unanswered", "registration_waiting"} {
		code := "closure_waitlist"
		if audience == "agreement_unanswered" {
			code = "closure_agreement_unanswered"
		}
		preview := closurePreview(f, audience, code, 0)
		f.send(1, fmt.Sprintf("/send %d", preview.Numbers[0]), "")
	}
	var jobs int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id IS NOT NULL").Scan(&jobs))
	if jobs != 0 {
		t.Fatal("absent campaign created participant jobs")
	}
}
