package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	pb "registration.local/backend/api"
)

// deliveryAllowed rechecks access to queued administrative output. Public
// survey messages and broadcast content are not administrative replies.
func (s *Service) deliveryAllowed(ctx context.Context, tx pgx.Tx, d *pb.Delivery) (bool, error) {
	permission := ""
	private := false
	switch d.Kind {
	case "export":
		permission, private = "table_viewer", true
	case "sync":
		permission, private = "admin", true
	case "view":
		switch d.View.Kind {
		case "help_participant": // Persisted by the previous implementation/migration.
			d.View = &pb.View{Kind: "help_public"}
		case "help", "permissions":
			permission, private = "operator", true
		case "stats", "sources":
			permission = "table_viewer"
		case "sync_done":
			permission, private = "admin", true
		case "broadcast_preview", "broadcast_progress":
			permission = "message_sender"
		case "notice":
			switch d.View.Code {
			case "denied", "unavailable":
				// Retired refusal codes: never replace a denial with another message.
				return false, nil
			case "sync_denied":
				permission, private = "admin", true
			case "sources_usage":
				permission = "table_viewer"
			case "send_source", "not_found", "invalid_status":
				permission = "message_sender"
			case "command_usage":
				permission = "operator"
			}
		}
	}
	if private && (d.Group || d.Chat != d.Actor) {
		return false, nil
	}
	if permission == "operator" {
		return s.operator(ctx, tx, d.Actor)
	}
	return permission == "" || s.allowed(ctx, tx, d.Actor, permission), nil
}
