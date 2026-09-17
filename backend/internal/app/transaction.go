package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type afterCommitKey struct{}
type afterCommitActions struct{ actions []func() }

func repositoryWrite[T any](ctx context.Context, s *Service, repo domain.ContentHash, fn func(context.Context) (T, error)) (out T, err error) {
	tx, ok := s.meta.(outbound.RepositoryTransactions)
	if !ok {
		return fn(ctx)
	}
	if _, nested := ctx.Value(afterCommitKey{}).(*afterCommitActions); nested {
		err = tx.WithinRepository(ctx, repo, func(bound context.Context) error { out, err = fn(bound); return err })
	} else {
		for attempt := 0; attempt < 3; attempt++ {
			after := &afterCommitActions{}
			err = tx.WithinRepository(ctx, repo, func(bound context.Context) error {
				out, err = fn(context.WithValue(bound, afterCommitKey{}, after))
				return err
			})
			if err == nil {
				for _, action := range after.actions {
					action()
				}
				break
			}
			// Only explicit PostgreSQL transaction-abort codes are retryable.
			// A lost COMMIT acknowledgement has an unknown outcome and must use
			// operation-specific idempotent replay instead of blind retry here.
			var state interface{ SQLState() string }
			if attempt == 2 || !errors.As(err, &state) || (state.SQLState() != "40001" && state.SQLState() != "40P01") {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func repositoryWriteError(ctx context.Context, s *Service, repo domain.ContentHash, fn func(context.Context) error) error {
	_, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (struct{}, error) { return struct{}{}, fn(bound) })
	return err
}

func repositoryRead[T any](ctx context.Context, s *Service, fn func(context.Context) (T, error)) (out T, err error) {
	if tx, ok := s.meta.(outbound.RepositoryTransactions); ok {
		err = tx.WithinReadSnapshot(ctx, func(bound context.Context) error { out, err = fn(bound); return err })
		if err != nil {
			var zero T
			return zero, err
		}
		return out, nil
	}
	return fn(ctx)
}

func afterRepositoryCommit(ctx context.Context, fn func()) {
	if after, ok := ctx.Value(afterCommitKey{}).(*afterCommitActions); ok {
		after.actions = append(after.actions, fn)
		return
	}
	fn()
}
