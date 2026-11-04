package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

const finalCapacity = 90

func finalInvitation() *pb.View {
	return &pb.View{Kind: "notice", Code: "final_invitation", Buttons: []*pb.Button{
		{LabelKey: "final_yes", Data: "final:yes"},
		{LabelKey: "final_no", Data: "final:no"},
	}}
}

// The campaign lock serializes every allocation and answer. Candidate reads do
// not lock other users: callers may already own their actor's questionnaire row.
// Invitations and their durable delivery jobs commit together.
func fillFinalConfirmations(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return err
	}
	campaign, err := readFinalCampaign(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, store.Q("final_exclude")); err != nil {
		return err
	}
	var occupied int
	if err = tx.QueryRow(ctx, store.Q("final_occupancy")).Scan(&occupied); err != nil {
		return err
	}
	if occupied >= finalCapacity {
		return nil
	}
	rows, err := tx.Query(ctx, store.Q("final_candidates"), campaign.question, campaign.positive, finalCapacity-occupied)
	if err != nil {
		return err
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	data, err := protojson.Marshal(finalInvitation())
	if err != nil {
		return err
	}
	for _, actor := range candidates {
		if _, err = tx.Exec(ctx, store.Q("final_invite"), actor); err != nil {
			return err
		}
		var delivery int64
		if err = tx.QueryRow(ctx, store.Q("final_delivery"), actor, data, carrier(ctx), campaign.sourceChat, campaign.sourceMessage).Scan(&delivery); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, store.Q("final_link_delivery"), actor, delivery); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) finalCallback(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies) (bool, error) {
	if !strings.HasPrefix(in.Callback, "final:") {
		return false, nil
	}
	if in.Callback != "final:yes" && in.Callback != "final:no" {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "notice", Code: "stale"}, in.Actor, 0, 0)
	}
	if _, err := tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	var state string
	var eligible bool
	err := tx.QueryRow(ctx, store.Q("final_invitation"), in.Actor).Scan(&state, &eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "notice", Code: "stale"}, in.Actor, 0, 0)
	}
	if err != nil {
		return true, err
	}
	if err = r.removeKeyboard(ctx, tx, in.Chat, false, in.Actor); err != nil {
		return true, err
	}
	// A first answer is final. In particular, a declined invitation cannot be
	// resurrected by an old Yes button after its place has gone to someone else.
	if state != "invited" {
		return true, nil
	}
	code := "final_" + strings.TrimPrefix(in.Callback, "final:") + "_saved"
	if !eligible {
		code = "final_ineligible"
	} else if _, err = tx.Exec(ctx, store.Q("final_answer"), in.Actor, strings.TrimPrefix(in.Callback, "final:")); err != nil {
		return true, err
	}
	if err = fillFinalConfirmations(ctx, tx); err != nil {
		return true, err
	}
	sends := *r
	sends.sendOnly = true
	return true, sends.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "notice", Code: code}, in.Actor, 0, 0)
}

// Recheck at Claim so retries cannot restore buttons after an answer or expose
// an invitation to someone who no longer satisfies the audience predicates.
func refreshFinalDelivery(ctx context.Context, tx pgx.Tx, d *pb.Delivery) (bool, error) {
	if d.View == nil || d.View.Code != "final_invitation" {
		return true, nil
	}
	if d.Kind == "copy" && d.View.Kind == "final_preview" {
		if len(d.View.Numbers) != 1 {
			return false, nil
		}
		draft, err := readFinalDraft(ctx, tx, d.Actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil || draft.id != d.View.Numbers[0] || draft.sourceMessage <= 0 {
			return false, err
		}
		d.SourceChat, d.SourceMessage = draft.sourceChat, draft.sourceMessage
		if draft.previewMessage > 0 {
			d.SourceChat, d.SourceMessage = draft.previewChat, draft.previewMessage
		}
		return true, nil
	}
	if d.Kind != "copy" && d.Kind != "view" {
		return true, nil
	}
	var state string
	var eligible bool
	err := tx.QueryRow(ctx, store.Q("final_invitation"), d.Chat).Scan(&state, &eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return state == "invited" && eligible, err
}
