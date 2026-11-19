package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) anchoredSessionStore() (outbound.AnchoredSessionStore, error) {
	store, ok := s.meta.(outbound.AnchoredSessionStore)
	if !ok {
		return nil, fmt.Errorf("%w: anchored session storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

func anchoredSessionKey(repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHashes(repo); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	return nil
}

// anchoredSessionActor returns the account a report is stored for. A report
// always belongs to a signed-in account; the server never files one itself.
func anchoredSessionActor(ctx context.Context) (string, error) {
	actor, system := inbound.RepositoryActor(ctx)
	if system || actor == "" {
		return "", domain.ErrForbidden
	}
	return actor, nil
}

// PutAnchoredSession stores the report a member's CLI computed for one
// session, replacing that session's earlier report. A session belongs to the
// account that first reported it. The same content again changes nothing.
//
// A report does not move the repository revision: nothing a browser shows
// reads it yet, so there is nothing for an open page to refetch.
func (s *Service) PutAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	if err := body.Validate(); err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	body = body.Canonical()
	hash, err := body.Hash()
	if err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_session.reported"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSessionReceipt, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		now := time.Now().UTC()
		next := domain.AnchoredSession{RepoID: repo, Provider: provider, SessionID: sessionID, Uploader: actor,
			Revision: 1, BodyHash: hash, CreatedAt: now, UpdatedAt: now, Body: body}
		existing, err := store.GetAnchoredSession(bound, repo, provider, sessionID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return domain.AnchoredSessionReceipt{}, err
		case existing.Uploader != actor:
			return domain.AnchoredSessionReceipt{}, fmt.Errorf("%w: this identifier is already reported by another account", domain.ErrConflict)
		case existing.BodyHash == hash:
			return domain.AnchoredSessionReceipt{Revision: existing.Revision, BodyHash: hash}, nil
		default:
			next.Revision, next.CreatedAt = existing.Revision+1, existing.CreatedAt
		}
		if err := store.PutAnchoredSession(bound, next); err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		return domain.AnchoredSessionReceipt{Revision: next.Revision, BodyHash: hash, Changed: true}, nil
	})
}

// GetAnchoredSession returns a report to the account that uploaded it. To
// anyone else it does not exist.
func (s *Service) GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSession{}, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) (domain.AnchoredSession, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		session, err := store.GetAnchoredSession(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		if session.Uploader != actor {
			return domain.AnchoredSession{}, domain.ErrNotFound
		}
		return session, nil
	})
}

// ListAnchoredSessions lists the caller's own reports.
func (s *Service) ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSessionSummary, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return nil, err
		}
		sessions, err := store.ListAnchoredSessions(bound, repo, actor)
		if err != nil {
			return nil, err
		}
		summaries := make([]domain.AnchoredSessionSummary, 0, len(sessions))
		for _, session := range sessions {
			summaries = append(summaries, session.Summary())
		}
		return summaries, nil
	})
}

// AnchoredCommitSegments returns the segments of the caller's own sessions
// that end at a commit.
func (s *Service) AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if !domain.ValidAnchoredCommitSHA(commitSHA) {
		return nil, fmt.Errorf("%w: commit number", domain.ErrValidation)
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredCommitSegment, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return nil, err
		}
		links, err := store.ListAnchoredCommitLinks(bound, repo, commitSHA)
		if err != nil {
			return nil, err
		}
		segments := []domain.AnchoredCommitSegment{}
		for _, link := range links {
			session, err := store.GetAnchoredSession(bound, repo, link.Provider, link.SessionID)
			if err != nil {
				return nil, err
			}
			if session.Uploader != actor {
				continue
			}
			if link.SegmentNo < 1 || link.SegmentNo > len(session.Body.Segments) {
				return nil, fmt.Errorf("%w: anchored commit index names a missing segment", domain.ErrIntegrity)
			}
			segments = append(segments, domain.AnchoredCommitSegment{Provider: link.Provider, SessionID: link.SessionID,
				SegmentNo: link.SegmentNo, Rule: link.Rule, Segment: session.Body.Segments[link.SegmentNo-1]})
		}
		return segments, nil
	})
}

var _ inbound.AnchoredSessions = (*Service)(nil)
