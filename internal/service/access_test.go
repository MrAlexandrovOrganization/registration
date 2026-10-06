package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

// Unexpected reads/writes panic via the embedded nil Tx. Only access checks and
// enqueues are supported, so a denied command cannot accidentally mutate data.
type accessTx struct {
	pgx.Tx
	permissions map[string]bool
	views       []*pb.View
}

type accessRow bool

func (r accessRow) Scan(dest ...any) error {
	*dest[0].(*bool) = bool(r)
	return nil
}

func (tx *accessTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	switch query {
	case store.Q("operator"):
		return accessRow(tx.permissions["admin"] || tx.permissions["table_viewer"] || tx.permissions["message_sender"])
	case store.Q("allowed"):
		return accessRow(tx.permissions[args[1].(string)])
	default:
		panic("unexpected query")
	}
}

func (tx *accessTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if query != store.Q("enqueue") {
		panic("unexpected mutation")
	}
	v := &pb.View{}
	if err := protojson.Unmarshal(args[4].([]byte), v); err != nil {
		return pgconn.CommandTag{}, err
	}
	tx.views = append(tx.views, v)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestInaccessibleCommandsAreSilent(t *testing.T) {
	s := &Service{RootID: 1}
	commands := []string{"/my_permissions", "/stats", "/sources invalid", "/export", "/grant_permission 99 admin", "/revoke_permission", "/register_staff_chat", "/register_counselor_chat", "/register_superuser_chat", "/sync_staff_chat", "/sync_counselor_chat", "/broadcast staff", "/poll staff", "/send 1", "/pause 1", "/resume 1", "/cancel_broadcast 1", "/retry 1", "/progress 1", "/unknown"}
	for _, permission := range []string{"", "staff"} {
		for _, chatType := range []string{"private", "supergroup"} {
			for _, command := range commands {
				t.Run(permission+"/"+chatType+"/"+command, func(t *testing.T) {
					tx := &accessTx{permissions: map[string]bool{permission: true}}
					u := &user{state: "name"}
					handled, err := s.admin(context.Background(), tx, &pb.Update{Actor: 2, Chat: 2, ChatType: chatType, Text: command}, u, nil)
					if err != nil || !handled || len(tx.views) != 0 || u.state != "name" {
						t.Fatalf("denied command had effects: handled=%v err=%v views=%v", handled, err, tx.views)
					}
				})
			}
		}
	}
	for _, tc := range []struct{ permission, command, chatType string }{
		{"table_viewer", "/broadcast all", "private"},
		{"table_viewer", "/poll all", "private"},
		{"message_sender", "/poll all", "supergroup"},
		{"message_sender", "/sources", "private"},
		{"admin", "/grant_permission 99 admin", "private"},
		{"admin", "/my_permissions", "supergroup"},
		{"table_viewer", "/export", "supergroup"},
		{"admin", "/sync_staff_chat", "supergroup"},
		{"admin", "/register_staff_chat", "private"},
	} {
		tx := &accessTx{permissions: map[string]bool{tc.permission: true}}
		handled, err := s.admin(context.Background(), tx, &pb.Update{Actor: 2, Chat: 2, ChatType: tc.chatType, Text: tc.command}, &user{}, nil)
		if err != nil || !handled || len(tx.views) != 0 {
			t.Fatalf("partial access not silent: %+v", tc)
		}
	}
}

func TestPublicCommandsAndAuthorizedValidationStillReply(t *testing.T) {
	s := &Service{RootID: 1}
	for _, tc := range []struct{ permission, command, kind, code string }{
		{"", "/help", "help_public", ""},
		{"staff", "/help", "help_public", ""},
		{"", "/about", "about", ""},
		{"", "/bring", "bring", ""},
		{"admin", "/help", "help", "help_registration"},
		{"table_viewer", "/sources invalid", "notice", "sources_usage"},
		{"admin", "/grant_permission", "notice", "command_usage"},
	} {
		tx := &accessTx{permissions: map[string]bool{tc.permission: true}}
		handled, err := s.admin(context.Background(), tx, &pb.Update{Actor: 2, Chat: 2, ChatType: "private", Text: tc.command}, &user{}, nil)
		if err != nil || !handled || len(tx.views) != 1 || tx.views[0].Kind != tc.kind || tx.views[0].Code != tc.code {
			t.Fatalf("missing ordinary response: %+v, %v", tc, tx.views)
		}
	}
}

func TestQueuedAdminAccess(t *testing.T) {
	s := &Service{RootID: 1}
	for _, tc := range []struct {
		kind, view, code, permission string
		private                      bool
	}{
		{"view", "help", "", "operator", true},
		{"view", "permissions", "", "operator", true},
		{"view", "stats", "", "table_viewer", false},
		{"view", "sources", "", "table_viewer", false},
		{"export", "", "", "table_viewer", true},
		{"sync", "", "", "admin", true},
		{"view", "sync_done", "", "admin", true},
		{"view", "broadcast_preview", "", "message_sender", false},
		{"view", "broadcast_progress", "", "message_sender", false},
		{"view", "notice", "sync_denied", "admin", true},
		{"view", "notice", "sources_usage", "table_viewer", false},
		{"view", "notice", "send_source", "message_sender", false},
		{"view", "notice", "command_usage", "operator", false},
	} {
		for _, grant := range []string{"", "staff", "admin", "table_viewer", "message_sender"} {
			for _, group := range []bool{false, true} {
				tx := &accessTx{permissions: map[string]bool{grant: true}}
				d := &pb.Delivery{Actor: 2, Chat: 2, Group: group, Kind: tc.kind, View: &pb.View{Kind: tc.view, Code: tc.code}}
				want := grant == tc.permission || tc.permission == "operator" && (grant == "admin" || grant == "table_viewer" || grant == "message_sender")
				want = want && !(tc.private && group)
				got, err := s.deliveryAllowed(context.Background(), tx, d)
				if err != nil || got != want {
					t.Fatalf("%+v grant=%s group=%v: got %v, %v", tc, grant, group, got, err)
				}
			}
		}
	}
	for _, code := range []string{"denied", "unavailable"} {
		got, err := s.deliveryAllowed(context.Background(), &accessTx{}, &pb.Delivery{Actor: 1, Chat: 1, Kind: "view", View: &pb.View{Kind: "notice", Code: code}})
		if err != nil || got {
			t.Fatal("legacy denial would be sent")
		}
	}
	for _, v := range []*pb.View{{Kind: "question", Field: "name"}, {Kind: "validation", Code: "invalid_date"}, {Kind: "notice", Code: "invalid_content"}, {Kind: "notice", Code: "saved"}, {Kind: "notice", Code: "stale"}, {Kind: "about"}, {Kind: "bring"}, {Kind: "poll"}, {Kind: "help_public"}, {Kind: "help_participant"}} {
		d := &pb.Delivery{Actor: 2, Chat: 2, Kind: "view", View: v}
		got, err := s.deliveryAllowed(context.Background(), &accessTx{}, d)
		if err != nil || !got || d.View.Kind == "help_participant" {
			t.Fatalf("ordinary reply lost: %v", v)
		}
	}
}
