package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/domain"
	"registration.local/backend/internal/observability"
	"registration.local/backend/internal/store"
)

type Service struct {
	pb.UnimplementedRegistrationServer
	DB                   *pgxpool.Pool
	BotID, RootID        int64
	Milestones           []int
	ParticipationEnabled bool
}

func (s *Service) next(values map[string]string) string {
	return domain.NextRegistration(values, s.ParticipationEnabled)
}

func persistenceError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	slog.LogAttrs(ctx, slog.LevelError, "persistence.failed", observability.ErrorAttrs(err)...)
	return status.Error(codes.Unavailable, "operation could not be persisted")
}
func (s *Service) allowed(ctx context.Context, tx pgx.Tx, actor int64, permission string) bool {
	if actor == s.RootID {
		return true
	}
	var ok bool
	return tx.QueryRow(ctx, store.Q("allowed"), actor, permission).Scan(&ok) == nil && ok
}

func (s *Service) operator(ctx context.Context, tx pgx.Tx, actor int64) (bool, error) {
	if actor == s.RootID {
		return true, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, store.Q("operator"), actor).Scan(&ok)
	return ok, err
}
func carrier(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c.Get("traceparent")
}
func enqueue(ctx context.Context, tx pgx.Tx, chat int64, group bool, kind string, view *pb.View, actor, sourceChat, sourceMessage int64) error {
	return (*replies)(nil).enqueue(ctx, tx, chat, group, kind, view, actor, sourceChat, sourceMessage)
}

// replies associates immediate effects with their durable inbound update.
// Background effects intentionally have no inbound association.
type replies struct {
	botID  int64
	update *pb.Update
}

func (r *replies) enqueue(ctx context.Context, tx pgx.Tx, chat int64, group bool, kind string, view *pb.View, actor, sourceChat, sourceMessage int64) error {
	data := []byte("{}")
	var err error
	if view != nil {
		data, err = protojson.Marshal(view)
		if err != nil {
			return err
		}
	}
	var botID, updateID *int64
	var editID int64
	if r != nil {
		botID, updateID = &r.botID, &r.update.Id
		if chat == r.update.Chat && !group && kind == "view" {
			editID = editTarget(r.update, view)
		}
	}
	_, err = tx.Exec(ctx, store.Q("enqueue"), chat, group, "interactive", kind, data, actor, sourceChat, sourceMessage, carrier(ctx), botID, updateID, editID)
	return err
}

func editTarget(u *pb.Update, view *pb.View) int64 {
	if !u.CallbackMessageEditable || u.Callback == "" || u.MessageId <= 0 || u.ChatType != "private" || u.Chat != u.Actor || view == nil {
		return 0
	}
	switch view.Kind {
	case "question":
		// Both contact sharing and cancellation use the phone reply keyboard.
		if view.Field == "phone" {
			return 0
		}
	case "confirm", "registered", "edit", "about", "bring":
	default:
		return 0
	}
	return u.MessageId
}

func updateReplies(ctx context.Context, tx pgx.Tx, botID, updateID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, store.Q("update_replies"), botID, updateID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func (s *Service) Accept(ctx context.Context, u *pb.Update) (*pb.Receipt, error) {
	internal := func(err error) error { return persistenceError(ctx, err) }
	if err := validateUpdateIdentity(s.BotID, u); err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, store.Q("dedup"), s.BotID, u.Id)
	if err != nil {
		return nil, internal(err)
	}
	if tag.RowsAffected() == 0 {
		ids, err := updateReplies(ctx, tx, s.BotID, u.Id)
		if err != nil {
			return nil, internal(err)
		}
		slog.InfoContext(ctx, "update.replayed", "reply_count", len(ids))
		return &pb.Receipt{Duplicate: true, DeliveryIds: ids}, nil
	}
	replies := &replies{botID: s.BotID, update: u}
	contentValid := validUpdateContent(u)
	if !contentValid {
		// Identity is trusted, but content is permanently unprocessable. Persist
		// only dedup + a neutral reply, never the offending data or domain effects.
		err = replies.enqueue(ctx, tx, u.Chat, u.ChatType != "private", "view", &pb.View{Kind: "notice", Code: "invalid_content"}, u.Actor, 0, 0)
	} else if u.Kind == "blocked" {
		if u.ChatType == "private" {
			v := 0
			if !u.MemberActive {
				v = 1
			}
			_, err = tx.Exec(ctx, store.Q("blocked"), u.Chat, v)
		}
	} else if u.Kind == "member" {
		err = s.member(ctx, tx, u)
	} else if u.ChatType != "private" {
		err = s.adminGroup(ctx, tx, u, replies)
	} else {
		_, err = tx.Exec(ctx, store.Q("ensure_user"), u.Actor, u.Username, u.DisplayName)
		if err == nil {
			err = s.private(ctx, tx, u, replies)
		}
	}
	if err != nil {
		return nil, internal(err)
	}
	ids, err := updateReplies(ctx, tx, s.BotID, u.Id)
	if err != nil {
		return nil, internal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	slog.InfoContext(ctx, "update.committed", "reply_count", len(ids), "content_rejected", !contentValid)
	return &pb.Receipt{DeliveryIds: ids}, nil
}

type user struct {
	state   string
	version int64
	values  map[string]string
}

func readUser(ctx context.Context, tx pgx.Tx, id int64) (user, error) {
	u := user{values: map[string]string{}}
	values := make([]string, len(domain.Fields))
	dest := []any{&u.state, &u.version}
	for i := range values {
		dest = append(dest, &values[i])
	}
	err := tx.QueryRow(ctx, store.Q("user"), id).Scan(dest...)
	for i, key := range domain.Fields {
		u.values[key] = values[i]
	}
	return u, err
}
func saveUser(ctx context.Context, tx pgx.Tx, id int64, u *user) error {
	args := []any{id, u.state}
	for _, f := range domain.Fields {
		args = append(args, u.values[f])
	}
	return tx.QueryRow(ctx, store.Q("save_user"), args...).Scan(&u.version)
}
func (s *Service) button(version int64, action, label string) *pb.Button {
	return &pb.Button{LabelKey: label, Data: fmt.Sprintf("c:%d:%s", version, action)}
}
func (s *Service) view(u user) *pb.View {
	v := &pb.View{Kind: "question", Field: u.state}
	switch u.state {
	case "registered", "confirm":
		v.Kind = u.state
		for _, f := range domain.RegistrationFields(s.ParticipationEnabled) {
			v.Fields = append(v.Fields, &pb.Field{Key: f, Value: u.values[f]})
		}
		if u.state == "confirm" {
			v.Buttons = append(v.Buttons, s.button(u.version, "confirm", "confirm"))
		}
		v.Buttons = append(v.Buttons, s.button(u.version, "edit", "edit"))
		v.Buttons = append(v.Buttons, s.infoButtons()...)
	case "edit":
		v.Kind = "edit"
		for _, f := range domain.RegistrationFields(s.ParticipationEnabled) {
			v.Buttons = append(v.Buttons, s.button(u.version, "edit_"+f, f))
		}
		v.Buttons = append(v.Buttons, s.button(u.version, "cancel", "cancel"))
	default:
		field := strings.TrimPrefix(u.state, "edit_")
		v.Field = field
		for i := range domain.Options[field] {
			v.Buttons = append(v.Buttons, s.button(u.version, "option_"+strconv.Itoa(i), field+"_"+strconv.Itoa(i)))
		}
		if strings.HasPrefix(u.state, "edit_") {
			v.Buttons = append(v.Buttons, s.button(u.version, "cancel", "cancel"))
		}
	}
	return v
}

func (s *Service) infoButtons() []*pb.Button {
	return []*pb.Button{
		{LabelKey: "about_button", Data: "info:about"},
		{LabelKey: "bring_button", Data: "info:bring"},
	}
}

func (s *Service) private(ctx context.Context, tx pgx.Tx, in *pb.Update, replies *replies) error {
	u, err := readUser(ctx, tx, in.Actor)
	if err != nil {
		return err
	}
	reply := func(v *pb.View) error { return replies.enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0) }
	source, isStart := startSource(in.Text)
	isStart = isStart && in.Callback == ""
	if isStart {
		if _, err = tx.Exec(ctx, store.Q("record_first_start"), in.Actor, source); err != nil {
			return err
		}
	}
	if strings.HasPrefix(in.Text, "/") {
		handled, err := s.admin(ctx, tx, in, &u, replies)
		if handled || err != nil {
			return err
		}
	}
	if in.Callback == "info:about" || in.Callback == "info:bring" {
		v := &pb.View{Kind: strings.TrimPrefix(in.Callback, "info:")}
		if !strings.HasPrefix(u.state, "broadcast_") {
			v.Buttons = append(v.Buttons, s.button(u.version, "begin", "back_to_form"))
		}
		v.Buttons = append(v.Buttons, s.infoButtons()...)
		return reply(v)
	}
	if strings.HasPrefix(u.state, "broadcast_") && !isStart {
		if in.Text == "/cancel" {
			u.state = s.next(u.values)
			if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
				return err
			}
			return reply(s.view(u))
		}
		if !s.allowed(ctx, tx, in.Actor, "message_sender") {
			return nil
		}
		if in.MessageId <= 0 {
			return reply(&pb.View{Kind: "notice", Code: "send_source"})
		}
		audience := strings.TrimPrefix(u.state, "broadcast_")
		var id, count int64
		if err = tx.QueryRow(ctx, store.Q("broadcast_create"), in.Actor, audience, "copy", in.Chat, in.MessageId).Scan(&id); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, store.Q("audience_count"), audience).Scan(&count); err != nil {
			return err
		}
		u.state = s.next(u.values)
		if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
			return err
		}
		if err = replies.enqueue(ctx, tx, in.Chat, false, "copy", nil, in.Actor, in.Chat, in.MessageId); err != nil {
			return err
		}
		return reply(&pb.View{Kind: "broadcast_preview", Numbers: []int64{id, count}})
	}
	if strings.HasPrefix(in.Callback, "p:") {
		if !s.ParticipationEnabled {
			return reply(&pb.View{Kind: "notice", Code: "stale"})
		}
		parts := strings.Split(in.Callback, ":")
		if len(parts) != 2 || (parts[1] != "0" && parts[1] != "1") {
			return reply(&pb.View{Kind: "notice", Code: "stale"})
		}
		i, _ := strconv.Atoi(parts[1])
		u.values["trip_attendance"] = domain.Options["trip_attendance"][i]
		if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
			return err
		}
		return reply(&pb.View{Kind: "notice", Code: "poll_saved"})
	}
	action := ""
	field := strings.TrimPrefix(u.state, "edit_")
	if !isStart && !s.ParticipationEnabled && (field == "will_drive" || field == "trip_attendance") {
		u.state = s.next(u.values)
		if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
			return err
		}
		return reply(s.view(u))
	}
	if in.Callback != "" {
		parts := strings.Split(in.Callback, ":")
		if len(parts) != 3 || parts[0] != "c" || parts[1] != strconv.FormatInt(u.version, 10) {
			return reply(&pb.View{Kind: "notice", Code: "stale"})
		}
		action = parts[2]
	}
	cancelPhoneEdit := u.state == "edit_phone" && (action == "cancel" || in.Text == "/cancel")
	switch {
	case action == "begin":
		return reply(s.view(u))
	case isStart || in.Text == "/cancel" || u.state == "new":
		if u.state != "registered" || s.next(u.values) != "confirm" {
			u.state = s.next(u.values)
		}
	case action == "edit" && (u.state == "registered" || u.state == "confirm"):
		u.state = "edit"
	case action == "cancel":
		u.state = s.next(u.values)
	case strings.HasPrefix(action, "edit_") && u.state == "edit":
		valid := false
		for _, f := range domain.RegistrationFields(s.ParticipationEnabled) {
			if action == "edit_"+f {
				valid = true
			}
		}
		if !valid {
			return reply(&pb.View{Kind: "notice", Code: "stale"})
		}
		u.state = action
	case action == "confirm" && u.state == "confirm":
		if s.next(u.values) != "confirm" {
			u.state = s.next(u.values)
		} else {
			u.state = "registered"
		}
	case u.state == "registered" || u.state == "confirm" || u.state == "edit":
		return reply(s.view(u))
	default:
		field := strings.TrimPrefix(u.state, "edit_")
		value := in.Text
		if options := domain.Options[field]; len(options) > 0 {
			if !strings.HasPrefix(action, "option_") {
				return reply(s.view(u))
			}
			i, e := strconv.Atoi(strings.TrimPrefix(action, "option_"))
			if e != nil || i < 0 || i >= len(options) {
				return reply(&pb.View{Kind: "notice", Code: "stale"})
			}
			value = options[i]
		} else if action != "" {
			return reply(&pb.View{Kind: "notice", Code: "stale"})
		}
		if field == "phone" && in.Phone != "" {
			if in.ContactOwner != in.Actor {
				v := s.view(u)
				v.Kind, v.Code = "validation", "own_contact"
				return reply(v)
			}
			value = in.Phone
		}
		value, err = domain.Normalize(field, value)
		if err != nil {
			// Both field and code are domain-controlled, never raw input.
			slog.DebugContext(ctx, "validation.rejected", "code", err.Error())
			v := s.view(u)
			v.Kind, v.Code = "validation", err.Error()
			return reply(v)
		}
		u.values[field] = value
		u.state = s.next(u.values)
	}
	if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
		return err
	}
	if u.state == "registered" {
		if err = s.milestones(ctx, tx); err != nil {
			return err
		}
	}
	if isStart {
		if err = reply(&pb.View{Kind: "welcome"}); err != nil {
			return err
		}
	}
	v := s.view(u)
	if action == "confirm" && u.state == "registered" {
		v.Code = "registration_completed"
		v.Fields = nil
	}
	if err = reply(v); err != nil {
		return err
	}
	if cancelPhoneEdit {
		// Editing the questionnaire cannot remove a reply keyboard. A separate
		// send-only notice clears it, including when the next view has inline buttons.
		return reply(&pb.View{Kind: "notice", Code: "edit_cancelled"})
	}
	return nil
}
func (s *Service) milestones(ctx context.Context, tx pgx.Tx) error {
	if !s.ParticipationEnabled {
		return nil
	}
	for _, n := range s.Milestones {
		var threshold int
		err := tx.QueryRow(ctx, store.Q("milestone"), n).Scan(&threshold)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var chat int64
		err = tx.QueryRow(ctx, store.Q("chat_for_role"), "staff").Scan(&chat)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err = enqueue(ctx, tx, chat, true, "view", &pb.View{Kind: "milestone", Numbers: []int64{int64(n)}}, 0, 0, 0); err != nil {
			return err
		}
	}
	return nil
}
func token() string { return rand.Text() }
