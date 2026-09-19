package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type SessionNoticeService struct {
	selections outbound.SessionNoticeSelectionReader
	cursors    outbound.SessionNoticeCursorStore
}

func NewSessionNoticeService(selections outbound.SessionNoticeSelectionReader, cursors outbound.SessionNoticeCursorStore) *SessionNoticeService {
	return &SessionNoticeService{selections: selections, cursors: cursors}
}

func (s *SessionNoticeService) DeliverSessionNotice(ctx context.Context, in inbound.SessionNoticeInput, emit func(domain.SessionNotice) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	first, err := s.selections.ReadNoticeSelection(ctx, in.Cwd)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = first.Validate(); err != nil {
		return err
	}
	scope := domain.SessionNoticeScope{RepoID: first.RepoID, WorktreeID: first.WorktreeID, Provider: in.Provider, SessionID: in.SessionID}
	if err = scope.Validate(); err != nil {
		return err
	}
	return s.cursors.WithSessionNoticeCursor(ctx, scope, func(last domain.ContentHash) (domain.ContentHash, error) {
		// Re-read after acquiring delivery ownership; queued hooks cannot emit a
		// stale selection prepared before a newer hook's successful delivery.
		current, err := s.selections.ReadNoticeSelection(ctx, in.Cwd)
		if err != nil {
			return "", err
		}
		if err = current.Validate(); err != nil {
			return "", err
		}
		if current != first {
			return "", domain.ErrSelectionChanged
		}
		id := current.ID()
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if id == last {
			return last, nil
		}
		if err = emit(domain.SessionNotice{ID: id, Selection: current}); err != nil {
			return "", err
		}
		// An interrupted acknowledgement is retryable with the same notice ID.
		if err = ctx.Err(); err != nil {
			return "", err
		}
		return id, nil
	})
}
