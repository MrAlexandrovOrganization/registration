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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/domain"
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

func internal(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		slog.Error("database operation failed", "sqlstate", pg.Code)
	}
	return status.Error(codes.Unavailable, "operation could not be persisted")
}
func (s *Service) allowed(ctx context.Context, tx pgx.Tx, actor int64, permission string) bool {
	if actor == s.RootID {
		return true
	}
	var ok bool
	return tx.QueryRow(ctx, store.Q("allowed"), actor, permission).Scan(&ok) == nil && ok
}
func carrier(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c.Get("traceparent")
}
func enqueue(ctx context.Context, tx pgx.Tx, chat int64, group bool, kind string, view *pb.View, actor, sourceChat, sourceMessage int64) error {
	data := []byte("{}")
	var err error
	if view != nil {
		data, err = protojson.Marshal(view)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, store.Q("enqueue"), chat, group, "interactive", kind, data, actor, sourceChat, sourceMessage, carrier(ctx))
	return err
}
func (s *Service) Accept(ctx context.Context, u *pb.Update) (*pb.Receipt, error) {
	if u.Id < 0 || u.Actor <= 0 || u.Chat == 0 || len(u.Text) > 8192 || len(u.Callback) > 64 || len(u.Username) > 128 || len(u.DisplayName) > 512 {
		return nil, status.Error(codes.InvalidArgument, "invalid update")
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
		return &pb.Receipt{Duplicate: true}, nil
	}
	if u.Kind == "blocked" {
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
		err = s.adminGroup(ctx, tx, u)
	} else {
		if u.Chat != u.Actor {
			return nil, status.Error(codes.InvalidArgument, "private chat identity mismatch")
		}
		_, err = tx.Exec(ctx, store.Q("ensure_user"), u.Actor, u.Username, u.DisplayName)
		if err == nil {
			err = s.private(ctx, tx, u)
		}
	}
	if err != nil {
		return nil, internal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &pb.Receipt{}, nil
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
func (s *Service) private(ctx context.Context, tx pgx.Tx, in *pb.Update) error {
	u, err := readUser(ctx, tx, in.Actor)
	if err != nil {
		return err
	}
	reply := func(v *pb.View) error { return enqueue(ctx, tx, in.Chat, false, "view", v, in.Actor, 0, 0) }
	source, isStart := startSource(in.Text)
	isStart = isStart && in.Callback == ""
	if isStart {
		if _, err = tx.Exec(ctx, store.Q("record_first_start"), in.Actor, source); err != nil {
			return err
		}
	}
	if strings.HasPrefix(in.Text, "/") {
		handled, err := s.admin(ctx, tx, in, &u)
		if handled || err != nil {
			return err
		}
	}
	if strings.HasPrefix(u.state, "broadcast_") && !isStart {
		if !s.allowed(ctx, tx, in.Actor, "message_sender") {
			return reply(&pb.View{Kind: "notice", Code: "denied"})
		}
		if in.Text == "/cancel" {
			u.state = s.next(u.values)
			if err = saveUser(ctx, tx, in.Actor, &u); err != nil {
				return err
			}
			return reply(s.view(u))
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
		if err = enqueue(ctx, tx, in.Chat, false, "copy", nil, in.Actor, in.Chat, in.MessageId); err != nil {
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
	if !s.ParticipationEnabled && (field == "will_drive" || field == "trip_attendance") {
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
	switch {
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
				return reply(&pb.View{Kind: "notice", Code: "own_contact"})
			}
			value = in.Phone
		}
		value, err = domain.Normalize(field, value)
		if err != nil {
			return reply(&pb.View{Kind: "validation", Field: field, Code: err.Error()})
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
	return reply(s.view(u))
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
