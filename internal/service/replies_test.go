package service

import (
	"testing"

	pb "registration.local/backend/api"
)

func TestEditTarget(t *testing.T) {
	base := func() *pb.Update {
		return &pb.Update{Actor: 10, Chat: 10, ChatType: "private", Callback: "c:1:edit", MessageId: 42, CallbackMessageEditable: true}
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.Update)
		view   *pb.View
		want   int64
	}{
		{"edit", nil, &pb.View{Kind: "edit"}, 42},
		{"confirm", nil, &pb.View{Kind: "confirm"}, 42},
		{"registered", nil, &pb.View{Kind: "registered"}, 42},
		{"question", nil, &pb.View{Kind: "question", Field: "name"}, 42},
		{"reply keyboard", nil, &pb.View{Kind: "question", Field: "phone"}, 0},
		{"notice", nil, &pb.View{Kind: "notice", Code: "stale"}, 0},
		{"poll", nil, &pb.View{Kind: "poll"}, 0},
		{"missing view", nil, nil, 0},
		{"foreign or media", func(u *pb.Update) { u.CallbackMessageEditable = false }, &pb.View{Kind: "edit"}, 0},
		{"group", func(u *pb.Update) { u.ChatType = "supergroup" }, &pb.View{Kind: "edit"}, 0},
		{"other chat", func(u *pb.Update) { u.Chat = 11 }, &pb.View{Kind: "edit"}, 0},
		{"not callback", func(u *pb.Update) { u.Callback = "" }, &pb.View{Kind: "edit"}, 0},
		{"inaccessible", func(u *pb.Update) { u.MessageId = 0 }, &pb.View{Kind: "edit"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := base()
			if tc.change != nil {
				tc.change(u)
			}
			if got := editTarget(u, tc.view); got != tc.want {
				t.Fatalf("edit target %d, want %d", got, tc.want)
			}
		})
	}
}
