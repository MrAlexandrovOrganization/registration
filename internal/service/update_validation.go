package service

import (
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
)

// Identity is required before deduplication, including on duplicate replay. Never
// acknowledge an update in an invalid bot namespace or with ambiguous routing.
func validateUpdateIdentity(botID int64, u *pb.Update) error {
	if botID <= 0 {
		return status.Error(codes.FailedPrecondition, "invalid bot identity")
	}
	if u == nil || u.Id < 0 || u.Actor <= 0 || u.Chat == 0 {
		return status.Error(codes.InvalidArgument, "invalid update identity")
	}
	switch u.ChatType {
	case "private":
		if u.Chat != u.Actor {
			return status.Error(codes.InvalidArgument, "private chat identity mismatch")
		}
	case "group", "supergroup", "channel":
	default:
		return status.Error(codes.InvalidArgument, "invalid chat type")
	}
	return nil
}

// Telegram message text is limited in characters, not UTF-8 bytes. Accept up to
// 4096 Unicode code points (also accommodates 4096 UTF-16 units). Domain-specific
// limits such as a 500-character survey answer are checked later by Normalize.
// Other string bounds are generous ingress limits, not domain validation.
func validUpdateContent(u *pb.Update) bool {
	for _, field := range []struct {
		value string
		limit int
	}{
		{u.Text, 4096},
		{u.Username, 128},
		{u.DisplayName, 512},
		{u.ChatTitle, 256},
		{u.Phone, 256},
		{u.Callback, 64},
	} {
		// PostgreSQL text/JSONB cannot store U+0000. Do not retry it forever or
		// silently truncate/repair content and accidentally execute a command.
		if !utf8.ValidString(field.value) || strings.ContainsRune(field.value, 0) || utf8.RuneCountInString(field.value) > field.limit {
			return false
		}
	}
	// callback_data is the deliberate exception: Telegram specifies 1..64 bytes.
	if len(u.Callback) > 64 {
		return false
	}
	switch u.Kind {
	case "", "message", "callback", "member", "blocked":
		return true
	default:
		return false
	}
}
