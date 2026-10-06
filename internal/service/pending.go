package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/store"
)

// PendingInteractive recovers unclaimed replies, expired leases, due retries and
// background interactive work. Discovery does not acquire or bypass a lease.
func (s *Service) PendingInteractive(ctx context.Context, r *pb.PendingInteractiveRequest) (*pb.Receipt, error) {
	limit := r.Limit
	if limit < 0 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "limit must be between 0 and 100")
	}
	if limit == 0 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, store.Q("pending_interactive"), limit)
	if err != nil {
		return nil, persistenceError(ctx, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, persistenceError(ctx, err)
	}
	return &pb.Receipt{DeliveryIds: ids}, nil
}
