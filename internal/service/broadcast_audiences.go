package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func specialBroadcastAudience(audience string) bool {
	return audience == "final_pending" || closureBroadcastCode(audience) != ""
}

func closureBroadcastCode(audience string) string {
	switch audience {
	case "final_unanswered", "final_waiting", "registration_waiting":
		return "closure_waitlist"
	case "agreement_unanswered":
		return "closure_agreement_unanswered"
	}
	return ""
}

// One SQL definition supplies the preview, /send snapshot and delivery check.
// actor=0 selects the whole audience; a positive actor checks one recipient.
func specialBroadcastRecipients(ctx context.Context, tx pgx.Tx, audience string, actor int64) ([]int64, error) {
	rows, err := tx.Query(ctx, store.Q("special_broadcast_recipients"), audience, actor)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func broadcastAudienceCount(ctx context.Context, tx pgx.Tx, audience string) (int64, error) {
	if specialBroadcastAudience(audience) {
		ids, err := specialBroadcastRecipients(ctx, tx, audience, 0)
		return int64(len(ids)), err
	}
	var count int64
	err := tx.QueryRow(ctx, store.Q("audience_count"), audience).Scan(&count)
	return count, err
}

func prepareClosureBroadcast(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, audience string) error {
	var id int64
	if err := tx.QueryRow(ctx, store.Q("broadcast_create"), in.Actor, audience, "closure", int64(0), int64(0)).Scan(&id); err != nil {
		return err
	}
	count, err := broadcastAudienceCount(ctx, tx, audience)
	if err != nil {
		return err
	}
	if err = r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "notice", Code: closureBroadcastCode(audience)}, in.Actor, 0, 0); err != nil {
		return err
	}
	return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "broadcast_preview", Field: audience, Numbers: []int64{id, count}}, in.Actor, 0, 0)
}
