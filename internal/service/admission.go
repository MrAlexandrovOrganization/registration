package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"registration.local/backend/internal/store"
)

type AdmissionClosure struct {
	Question      int64     `json:"question"`
	ClosedAt      time.Time `json:"closed_at"`
	Reserve       int64     `json:"reserve"`
	AlreadyClosed bool      `json:"already_closed"`
}

// CloseFinalAdmission hides the source agreement from new forms and freezes
// eligibility at the cutoff. It neither sends invitations nor changes answers.
// The lifecycle lock fences concurrent form/broadcast answers before the cutoff;
// the campaign lock fences allocation and participant membership updates.
func (s *Service) CloseFinalAdmission(ctx context.Context, question int64) (AdmissionClosure, error) {
	result := AdmissionClosure{Question: question}
	if question <= 0 || s.RootID <= 0 {
		return result, status.Error(codes.InvalidArgument, "question and root actor are required")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, persistenceError(ctx, err)
	}
	defer tx.Rollback(ctx)
	for _, lock := range []string{"survey_lifecycle_write", "final_lock"} {
		if _, err = tx.Exec(ctx, store.Q(lock)); err != nil {
			return result, persistenceError(ctx, err)
		}
	}
	var previous *time.Time
	err = tx.QueryRow(ctx, store.Q("final_admission_state"), question).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, status.Error(codes.FailedPrecondition, "question does not match the active campaign")
	}
	if err != nil {
		return result, persistenceError(ctx, err)
	}
	result.AlreadyClosed = previous != nil
	if err = tx.QueryRow(ctx, store.Q("final_admission_close"), question).Scan(&result.ClosedAt); err != nil {
		return result, persistenceError(ctx, err)
	}
	if err = tx.QueryRow(ctx, store.Q("final_admission_reserve")).Scan(&result.Reserve); err != nil {
		return result, persistenceError(ctx, err)
	}
	if !result.AlreadyClosed {
		if _, err = tx.Exec(ctx, store.Q("audit"), s.RootID, "final_admission_closed", question); err != nil {
			return result, persistenceError(ctx, err)
		}
	}
	return result, persistenceError(ctx, tx.Commit(ctx))
}
