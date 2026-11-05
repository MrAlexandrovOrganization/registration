package service

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

const maxFinalExtra = 500

func (s *Service) finalExtraAllowed(ctx context.Context, tx pgx.Tx, actor int64) bool {
	return s.allowed(ctx, tx, actor, "admin") && s.allowed(ctx, tx, actor, "message_sender")
}

func finalExtraReply(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, code string) error {
	return r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_invites_result", Code: code}, in.Actor, 0, 0)
}

// Additional invitations use the campaign's reviewed message and do not reserve
// seats. A delivered operator preview authorizes one bounded set of recipients.
func (s *Service) finalExtraPreview(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies, parts []string) (bool, error) {
	if in.ChatType != "private" || !s.finalExtraAllowed(ctx, tx, in.Actor) {
		return true, nil
	}
	reply := func(code string) (bool, error) { return true, finalExtraReply(ctx, tx, in, r, code) }
	if len(parts) != 2 {
		return reply("final_extra_usage")
	}
	if _, err := tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	campaign, err := readFinalCampaign(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) {
		return reply("final_not_started")
	}
	if err != nil {
		return true, err
	}
	v := &pb.View{Kind: "final_invites_preview", Code: "final_more_preview", Field: "queue"}
	if strings.Split(parts[0], "@")[0] == "/final_more" {
		count, err := strconv.Atoi(parts[1])
		if err != nil || count < 1 || count > maxFinalExtra {
			return reply("final_extra_usage")
		}
		rows, err := tx.Query(ctx, store.Q("final_candidates"), campaign.question, campaign.positive, count)
		if err != nil {
			return true, err
		}
		v.Numbers, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return true, err
		}
	} else {
		target, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			username := strings.TrimPrefix(parts[1], "@")
			if len(username) == 0 || len(username) > 32 || strings.IndexFunc(username, func(r rune) bool {
				return r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
			}) >= 0 {
				return reply("final_extra_id_required")
			}
			rows, err := tx.Query(ctx, store.Q("final_decline_username"), username)
			if err != nil {
				return true, err
			}
			matches, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return true, err
			}
			if len(matches) != 1 {
				return reply("final_extra_id_required")
			}
			target = matches[0]
		}
		if target <= 0 {
			return reply("final_extra_usage")
		}
		var state, name string
		var eligible bool
		err = tx.QueryRow(ctx, store.Q("final_extra_target"), target).Scan(&state, &name, &eligible)
		if errors.Is(err, pgx.ErrNoRows) {
			return reply("final_extra_id_required")
		}
		if err != nil {
			return true, err
		}
		if state != "" {
			return reply("final_extra_already_invited")
		}
		if !eligible {
			return reply("final_extra_ineligible")
		}
		v.Code, v.Field, v.Numbers = "final_personal_preview", "manual", []int64{target}
		v.Fields = append(v.Fields, &pb.Field{Key: "final_decline_person", Value: name}, &pb.Field{Key: "final_decline_id", Value: number(target)})
	}
	if len(v.Numbers) == 0 {
		return reply("final_extra_empty")
	}
	var accepted int64
	if err = tx.QueryRow(ctx, store.Q("final_accepted")).Scan(&accepted); err != nil {
		return true, err
	}
	v.Fields = append(v.Fields, &pb.Field{Key: "final_extra_count", Value: number(int64(len(v.Numbers)))}, &pb.Field{Key: "final_free_seats", Value: number(max(0, int64(finalCapacity)-accepted))})
	v.Buttons = []*pb.Button{
		{LabelKey: "final_extra_send", Data: "final_extra:" + number(in.Id)},
		{LabelKey: "cancel", Data: "final_extra_cancel:" + number(in.Id)},
	}
	message := finalInvitation(campaign.positiveLabel, campaign.negativeLabel)
	message.Kind, message.Code = "final_invites_message_preview", ""
	for i, button := range message.Buttons {
		button.Data = "preview_final_" + number(int64(i))
	}
	if err = r.enqueue(ctx, tx, in.Chat, false, "copy", message, in.Actor, campaign.sourceChat, campaign.sourceMessage); err != nil {
		return true, err
	}
	return true, r.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0)
}

func (s *Service) finalExtraCallback(ctx context.Context, tx pgx.Tx, in *pb.Update, r *replies) (bool, error) {
	if !strings.HasPrefix(in.Callback, "final_extra:") && !strings.HasPrefix(in.Callback, "final_extra_cancel:") {
		return false, nil
	}
	if in.ChatType != "private" || !s.finalExtraAllowed(ctx, tx, in.Actor) {
		return true, nil
	}
	reply := func(code string) (bool, error) { return true, finalExtraReply(ctx, tx, in, r, code) }
	var body []byte
	err := tx.QueryRow(ctx, store.Q("final_extra_consume_preview"), in.Actor, in.MessageId, in.Callback).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return reply("final_extra_stale")
	}
	if err != nil {
		return true, err
	}
	if err = r.removeKeyboard(ctx, tx, in.Chat, false, in.Actor); err != nil {
		return true, err
	}
	if strings.HasPrefix(in.Callback, "final_extra_cancel:") {
		return reply("final_extra_cancelled")
	}
	v := &pb.View{}
	if err = protojson.Unmarshal(body, v); err != nil {
		return true, err
	}
	if len(v.Numbers) == 0 || len(v.Numbers) > maxFinalExtra || (v.Field != "manual" && v.Field != "queue") || v.Field == "manual" && len(v.Numbers) != 1 {
		return reply("final_extra_stale")
	}
	if _, err = tx.Exec(ctx, store.Q("final_lock")); err != nil {
		return true, err
	}
	campaign, err := readFinalCampaign(ctx, tx)
	if err != nil {
		return true, err
	}
	data, err := protojson.Marshal(finalInvitation(campaign.positiveLabel, campaign.negativeLabel))
	if err != nil {
		return true, err
	}
	var sent int64
	for _, target := range v.Numbers {
		var actor int64
		err = tx.QueryRow(ctx, store.Q("final_extra_invite"), target, v.Field == "manual").Scan(&actor)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return true, err
		}
		var job int64
		if err = tx.QueryRow(ctx, store.Q("final_delivery"), actor, data, carrier(ctx), campaign.sourceChat, campaign.sourceMessage).Scan(&job); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("final_link_delivery"), actor, job); err != nil {
			return true, err
		}
		sent++
	}
	auditTarget := sent
	if v.Field == "manual" {
		auditTarget = v.Numbers[0]
	}
	if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, "final_extra_"+v.Field, auditTarget); err != nil {
		return true, err
	}
	return true, r.enqueue(ctx, tx, in.Chat, false, "view", &pb.View{Kind: "final_invites_result", Code: "final_extra_done", Fields: []*pb.Field{{Key: "final_extra_count", Value: number(sent)}}}, in.Actor, 0, 0)
}
