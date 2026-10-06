package service

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
)

func TestUnicodeIngressAndPermanentContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update *pb.Update
		valid  bool
	}{
		{"Chinese regression 9000 bytes", &pb.Update{Text: strings.Repeat("中", 3000)}, true},
		{"4096 Chinese", &pb.Update{Text: strings.Repeat("中", 4096)}, true},
		{"4096 emoji", &pb.Update{Text: strings.Repeat("😀", 4096)}, true},
		{"4096 ASCII", &pb.Update{Text: strings.Repeat("a", 4096)}, true},
		{"4097 characters", &pb.Update{Text: strings.Repeat("中", 4097)}, false},
		{"oversized ASCII", &pb.Update{Text: strings.Repeat("a", 9000)}, false},
		{"Unicode metadata", &pb.Update{DisplayName: strings.Repeat("中", 256), ChatTitle: strings.Repeat("中", 255)}, true},
		{"oversized name", &pb.Update{DisplayName: strings.Repeat("a", 513)}, false},
		{"oversized username", &pb.Update{Username: strings.Repeat("a", 129)}, false},
		{"oversized phone", &pb.Update{Phone: strings.Repeat("1", 257)}, false},
		{"oversized title", &pb.Update{ChatTitle: strings.Repeat("a", 257)}, false},
		{"callback 64 bytes", &pb.Update{Callback: strings.Repeat("😀", 16)}, true},
		{"callback over 64 bytes", &pb.Update{Callback: strings.Repeat("中", 22)}, false},
		{"text NUL", &pb.Update{Text: "hello\x00"}, false},
		{"name NUL", &pb.Update{DisplayName: "name\x00"}, false},
		{"username NUL", &pb.Update{Username: "user\x00"}, false},
		{"title NUL", &pb.Update{ChatTitle: "title\x00"}, false},
		{"phone NUL", &pb.Update{Phone: "123\x00"}, false},
		{"callback NUL", &pb.Update{Callback: "c:1:edit\x00"}, false},
		{"invalid UTF8", &pb.Update{Text: string([]byte{0xff})}, false},
		{"unknown kind", &pb.Update{Kind: "invalid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validUpdateContent(tc.update); got != tc.valid {
				t.Fatalf("valid=%v, want %v", got, tc.valid)
			}
		})
	}
}

func TestAcceptIdentityBeforeDatabase(t *testing.T) {
	// Nil DB ensures invalid identity cannot reach dedup, even for poison content.
	for _, tc := range []struct {
		name   string
		botID  int64
		update *pb.Update
		code   codes.Code
	}{
		{"bot namespace", 0, &pb.Update{Id: 1, Actor: 10, Chat: 10, ChatType: "private"}, codes.FailedPrecondition},
		{"nil update", 1000, nil, codes.InvalidArgument},
		{"negative update", 1000, &pb.Update{Id: -1, Actor: 10, Chat: 10, ChatType: "private"}, codes.InvalidArgument},
		{"actor", 1000, &pb.Update{Id: 1, Chat: 10, ChatType: "private"}, codes.InvalidArgument},
		{"chat", 1000, &pb.Update{Id: 1, Actor: 10, ChatType: "private"}, codes.InvalidArgument},
		{"private mismatch", 1000, &pb.Update{Id: 1, Actor: 10, Chat: 11, ChatType: "private", Text: strings.Repeat("中", 4097)}, codes.InvalidArgument},
		{"blocked private mismatch", 1000, &pb.Update{Id: 1, Actor: 10, Chat: 11, ChatType: "private", Kind: "blocked"}, codes.InvalidArgument},
		{"routing", 1000, &pb.Update{Id: 1, Actor: 10, Chat: 10, ChatType: "invalid"}, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := (&Service{BotID: tc.botID}).Accept(context.Background(), tc.update)
			if r != nil || status.Code(err) != tc.code {
				t.Fatalf("receipt=%v code=%v", r, status.Code(err))
			}
		})
	}
}
