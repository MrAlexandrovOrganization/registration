package service

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

func (s *Service) ReleaseSender(ctx context.Context, r *pb.ReleaseSenderRequest) (*pb.ReleaseSenderResponse, error) {
	if len(r.GetWorker()) < 8 || len(r.GetWorker()) > 100 {
		return nil, status.Error(codes.InvalidArgument, "invalid sender identity")
	}
	// Atomic ownership check serializes with Claim. Never reset cooldown or job
	// leases: a missing completion still needs normal uncertain-delivery recovery.
	result, err := s.DB.Exec(ctx, store.Q("sender_release"), r.Worker)
	if err != nil {
		return nil, persistenceError(ctx, err)
	}
	return &pb.ReleaseSenderResponse{Released: result.RowsAffected() == 1}, nil
}
