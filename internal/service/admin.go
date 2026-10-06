package service

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func audienceValid(v string) bool {
	switch v {
	case "all", "registered", "incomplete", "yes", "maybe", "staff", "counselor":
		return true
	}
	return false
}
func permissionValid(v string) bool {
	switch v {
	case "admin", "table_viewer", "message_sender", "staff":
		return true
	}
	return false
}
func (s *Service) admin(ctx context.Context, tx pgx.Tx, in *pb.Update, u *user, replies *replies) (bool, error) {
	parts := strings.Fields(in.Text)
	if len(parts) == 0 {
		return false, nil
	}
	cmd := strings.Split(parts[0], "@")[0]
	reply := func(v *pb.View) (bool, error) {
		return true, replies.enqueue(ctx, tx, in.Chat, in.ChatType != "private", "view", v, in.Actor, 0, 0)
	}
	notice := func(code string) (bool, error) {
		if code == "denied" {
			return true, nil
		}
		return reply(&pb.View{Kind: "notice", Code: code})
	}
	// Public help and unknown/privileged commands must not reveal operator roles.
	operator, err := s.operator(ctx, tx, in.Actor)
	if err != nil {
		return true, err
	}
	if !operator {
		switch cmd {
		case "/help":
			return reply(&pb.View{Kind: "help_public"})
		case "/start", "/cancel", "/about", "/bring":
		default:
			return true, nil
		}
	}
	allowed := func(p string) bool { return s.allowed(ctx, tx, in.Actor, p) }
	switch cmd {
	case "/help":
		if in.ChatType != "private" {
			return reply(&pb.View{Kind: "help_public"})
		}
		if !s.ParticipationEnabled {
			return reply(&pb.View{Kind: "help", Code: "help_registration"})
		}
		return reply(&pb.View{Kind: "help"})
	case "/about":
		return reply(&pb.View{Kind: "about"})
	case "/bring":
		return reply(&pb.View{Kind: "bring"})
	case "/my_permissions":
		if in.ChatType != "private" {
			return true, nil
		}
		v := &pb.View{Kind: "permissions"}
		if in.Actor == s.RootID {
			v.Fields = append(v.Fields, &pb.Field{Key: "permission", Value: "root"})
		}
		rows, err := tx.Query(ctx, store.Q("permissions"), in.Actor)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				return true, err
			}
			v.Fields = append(v.Fields, &pb.Field{Key: "permission", Value: p})
		}
		if err = rows.Err(); err != nil {
			return true, err
		}
		return reply(v)
	case "/stats":
		if !allowed("table_viewer") {
			return notice("denied")
		}
		v := &pb.View{Kind: "stats", Numbers: make([]int64, 8)}
		dest := make([]any, 8)
		for i := range dest {
			dest[i] = &v.Numbers[i]
		}
		if err := tx.QueryRow(ctx, store.Q("stats")).Scan(dest...); err != nil {
			return true, err
		}
		if !s.ParticipationEnabled {
			for _, i := range []int{0, 1, 2, 7} {
				v.Fields = append(v.Fields, &pb.Field{Key: "stats_" + strconv.Itoa(i), Value: strconv.FormatInt(v.Numbers[i], 10)})
			}
			v.Numbers = nil
		}
		return reply(v)
	case "/sources":
		if !allowed("table_viewer") {
			return notice("denied")
		}
		page := int64(1)
		if len(parts) > 2 {
			return notice("sources_usage")
		}
		if len(parts) == 2 {
			var err error
			page, err = strconv.ParseInt(parts[1], 10, 32)
			if err != nil || page < 1 {
				return notice("sources_usage")
			}
		}
		v := &pb.View{Kind: "sources", Numbers: []int64{page}}
		rows, err := tx.Query(ctx, store.Q("sources"), (page-1)*20)
		if err != nil {
			return true, err
		}
		for rows.Next() {
			var source string
			var count int64
			if err = rows.Scan(&source, &count); err != nil {
				rows.Close()
				return true, err
			}
			if len(v.Fields) == 20 {
				v.Numbers = append(v.Numbers, page+1)
				break
			}
			v.Fields = append(v.Fields, &pb.Field{Key: source, Value: strconv.FormatInt(count, 10)})
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return true, err
		}
		return reply(v)
	case "/export":
		if !allowed("table_viewer") || in.ChatType != "private" {
			return notice("denied")
		}
		return true, replies.enqueue(ctx, tx, in.Chat, false, "export", nil, in.Actor, 0, 0)
	case "/sync_staff_chat", "/sync_counselor_chat":
		if !allowed("admin") || in.ChatType != "private" {
			return notice("denied")
		}
		role := strings.TrimSuffix(strings.TrimPrefix(cmd, "/sync_"), "_chat")
		return true, replies.enqueue(ctx, tx, in.Chat, false, "sync", &pb.View{Code: role, Numbers: []int64{0, 0, 0}}, in.Actor, 0, 0)
	case "/grant_permission", "/revoke_permission":
		if !allowed("admin") {
			return notice("denied")
		}
		if len(parts) != 3 || !permissionValid(parts[2]) {
			return notice("command_usage")
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return notice("command_usage")
		}
		if parts[2] == "admin" && in.Actor != s.RootID {
			return notice("denied")
		}
		if cmd == "/grant_permission" {
			_, err = tx.Exec(ctx, store.Q("grant"), id, parts[2], in.Actor)
		} else {
			_, err = tx.Exec(ctx, store.Q("revoke"), id, parts[2])
		}
		if err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, cmd+":"+parts[2], id); err != nil {
			return true, err
		}
		return notice("saved")
	case "/register_staff_chat", "/register_counselor_chat", "/register_superuser_chat":
		if !allowed("admin") || in.ChatType == "private" {
			return notice("denied")
		}
		role := strings.TrimSuffix(strings.TrimPrefix(cmd, "/register_"), "_chat")
		if _, err := tx.Exec(ctx, store.Q("chat_deactivate"), role, in.Chat); err != nil {
			return true, err
		}
		if _, err := tx.Exec(ctx, store.Q("chat_register"), in.Chat, role, in.ChatTitle); err != nil {
			return true, err
		}
		return notice("saved")
	case "/broadcast":
		if !allowed("message_sender") || u == nil {
			return notice("denied")
		}
		if len(parts) != 2 || !audienceValid(parts[1]) {
			return notice("command_usage")
		}
		if !s.ParticipationEnabled && (parts[1] == "yes" || parts[1] == "maybe") {
			return notice("invalid_status")
		}
		u.state = "broadcast_" + parts[1]
		if err := saveUser(ctx, tx, in.Actor, u); err != nil {
			return true, err
		}
		return notice("send_source")
	case "/poll":
		if !allowed("message_sender") || in.ChatType != "private" {
			return notice("denied")
		}
		if !s.ParticipationEnabled {
			return notice("invalid_status")
		}
		if len(parts) != 2 || !audienceValid(parts[1]) {
			return notice("command_usage")
		}
		var id, count int64
		if err := tx.QueryRow(ctx, store.Q("broadcast_create"), in.Actor, parts[1], "view", 0, 0).Scan(&id); err != nil {
			return true, err
		}
		if err := tx.QueryRow(ctx, store.Q("audience_count"), parts[1]).Scan(&count); err != nil {
			return true, err
		}
		if err := replies.enqueue(ctx, tx, in.Chat, false, "view", s.pollView(), in.Actor, 0, 0); err != nil {
			return true, err
		}
		return reply(&pb.View{Kind: "broadcast_preview", Numbers: []int64{id, count}})
	case "/send", "/pause", "/resume", "/cancel_broadcast", "/retry", "/progress":
		if !allowed("message_sender") {
			return notice("denied")
		}
		if len(parts) != 2 {
			return notice("command_usage")
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return notice("command_usage")
		}
		var actor, sourceChat, sourceMessage int64
		var audience, kind, state string
		err = tx.QueryRow(ctx, store.Q("broadcast_get"), id).Scan(&actor, &audience, &kind, &sourceChat, &sourceMessage, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return notice("not_found")
		}
		if err != nil {
			return true, err
		}
		if actor != in.Actor && !allowed("admin") {
			return notice("denied")
		}
		if !s.ParticipationEnabled && (kind == "view" || audience == "yes" || audience == "maybe") && (cmd == "/send" || cmd == "/resume" || cmd == "/retry") {
			return notice("invalid_status")
		}
		switch cmd {
		case "/send":
			if state != "draft" {
				return notice("invalid_status")
			}
			body := []byte("{}")
			if kind == "view" {
				body, err = protojson.Marshal(s.pollView())
				if err != nil {
					return true, err
				}
			}
			if _, err = tx.Exec(ctx, store.Q("broadcast_start"), id, audience, kind, body, actor, sourceChat, sourceMessage, carrier(ctx)); err != nil {
				return true, err
			}
			state = "running"
		case "/pause":
			if state != "running" {
				return notice("invalid_status")
			}
			state = "paused"
		case "/resume":
			if state != "paused" {
				return notice("invalid_status")
			}
			state = "running"
		case "/cancel_broadcast":
			state = "cancelled"
			if _, err = tx.Exec(ctx, store.Q("broadcast_cancel"), id); err != nil {
				return true, err
			}
		case "/retry":
			if state == "cancelled" || state == "draft" {
				return notice("invalid_status")
			}
			if _, err = tx.Exec(ctx, store.Q("broadcast_retry"), id); err != nil {
				return true, err
			}
			state = "running"
		}
		if _, err = tx.Exec(ctx, store.Q("broadcast_status"), id, state); err != nil {
			return true, err
		}
		if cmd != "/progress" {
			if _, err = tx.Exec(ctx, store.Q("audit"), in.Actor, cmd, id); err != nil {
				return true, err
			}
		}
		v := &pb.View{Kind: "broadcast_progress", Numbers: []int64{id}, Code: state}
		rows, err := tx.Query(ctx, store.Q("broadcast_counts"), id)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var count int64
			if err = rows.Scan(&status, &count); err != nil {
				return true, err
			}
			v.Fields = append(v.Fields, &pb.Field{Key: status, Value: strconv.FormatInt(count, 10)})
		}
		if err = rows.Err(); err != nil {
			return true, err
		}
		return reply(v)
	}
	if cmd == "/start" || cmd == "/cancel" {
		return false, nil
	}
	return true, nil
}
func (s *Service) pollView() *pb.View {
	return &pb.View{Kind: "poll", Buttons: []*pb.Button{
		{LabelKey: "trip_attendance_0", Data: "p:0"},
		{LabelKey: "trip_attendance_1", Data: "p:1"},
	}}
}
func (s *Service) adminGroup(ctx context.Context, tx pgx.Tx, in *pb.Update, replies *replies) error {
	if !strings.HasPrefix(in.Text, "/") {
		return nil
	}
	_, err := s.admin(ctx, tx, in, nil, replies)
	return err
}
func (s *Service) member(ctx context.Context, tx pgx.Tx, in *pb.Update) error {
	if in.MemberId <= 0 {
		return nil
	}
	var role string
	err := tx.QueryRow(ctx, store.Q("chat_type"), in.Chat).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if role != "staff" && role != "counselor" {
		return nil
	}
	active := 0
	if in.MemberActive {
		active = 1
	}
	if _, err = tx.Exec(ctx, store.Q("role_update"), in.MemberId, role, active); err != nil {
		return err
	}
	// Chat membership sets event flags only. Explicit administrative grants remain independent.
	return nil
}
func (s *Service) Members(ctx context.Context, r *pb.MembersRequest) (*pb.MembersResponse, error) {
	internal := func(err error) error { return persistenceError(ctx, err) }
	if r.Role != "staff" && r.Role != "counselor" {
		return nil, status.Error(codes.InvalidArgument, "invalid role")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	if !s.allowed(ctx, tx, r.Actor, "admin") {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	result := &pb.MembersResponse{}
	if err = tx.QueryRow(ctx, store.Q("chat_for_role"), r.Role).Scan(&result.Chat); err != nil {
		return nil, status.Error(codes.NotFound, "chat not configured")
	}
	rows, err := tx.Query(ctx, store.Q("members"), r.After)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, internal(err)
		}
		result.Users = append(result.Users, id)
	}
	return result, internal(rows.Err())
}

func (s *Service) SyncMember(ctx context.Context, r *pb.SyncMemberRequest) (*pb.Receipt, error) {
	internal := func(err error) error { return persistenceError(ctx, err) }
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	if !s.allowed(ctx, tx, r.Actor, "admin") {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	if err = s.member(ctx, tx, &pb.Update{Chat: r.Chat, MemberId: r.User, MemberActive: r.Active}); err != nil {
		return nil, internal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &pb.Receipt{}, nil
}
