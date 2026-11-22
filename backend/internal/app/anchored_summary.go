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

// anchoredSummaryCommits is the most commits one read of summaries names.
const anchoredSummaryCommits = 200

func (s *Service) anchoredSummaryStore() (outbound.AnchoredSummaryStore, error) {
	store, ok := s.meta.(outbound.AnchoredSummaryStore)
	if !ok {
		return nil, fmt.Errorf("%w: anchored summary storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// PutAnchoredSummary stores the summary a member's CLI made of one of that
// member's sessions at a commit. The session's report must hold a segment
// that ends at the commit: a summary is what was done up to a commit, and a
// session that made none has no summary. The same text again changes nothing.
func (s *Service) PutAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) (domain.AnchoredSummaryReceipt, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	if !domain.ValidAnchoredCommitSHA(commit) {
		return domain.AnchoredSummaryReceipt{}, fmt.Errorf("%w: a summary is stored at a full commit number", domain.ErrValidation)
	}
	body = body.Canonical()
	if err := body.Validate(); err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	hash, err := body.Hash()
	if err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_summary.stored"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSummaryReceipt, error) {
		sessions, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		summaries, err := s.anchoredSummaryStore()
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		session, err := sessions.GetAnchoredSession(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		if session.Uploader != actor {
			// Another account's session is not there to be written to.
			return domain.AnchoredSummaryReceipt{}, domain.ErrNotFound
		}
		if session.Body.SegmentOfCommit(commit) == 0 {
			return domain.AnchoredSummaryReceipt{}, fmt.Errorf("%w: no segment of the session ends at that commit", domain.ErrValidation)
		}
		now := time.Now().UTC()
		next := domain.AnchoredSummary{RepoID: repo, Provider: provider, SessionID: sessionID, Commit: commit, BodyHash: hash, Body: body, CreatedAt: now, UpdatedAt: now}
		existing, err := summaries.GetAnchoredSummary(bound, repo, provider, sessionID, commit)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return domain.AnchoredSummaryReceipt{}, err
		case existing.BodyHash == hash:
			return domain.AnchoredSummaryReceipt{BodyHash: hash}, nil
		default:
			next.CreatedAt = existing.CreatedAt
		}
		if err := summaries.PutAnchoredSummary(bound, next); err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		return domain.AnchoredSummaryReceipt{BodyHash: hash, Changed: true}, nil
	})
}

// ListAnchoredSummaries lists the summaries at the commits that the caller
// may read, by commit, tool, then session. A summary is read under the rule
// of the segment it belongs to: by the session's uploader always, by anyone
// else when that segment is open.
func (s *Service) ListAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if len(commits) == 0 || len(commits) > anchoredSummaryCommits {
		return nil, fmt.Errorf("%w: summaries are read for 1 to %d commits", domain.ErrValidation, anchoredSummaryCommits)
	}
	for _, commit := range commits {
		if !domain.ValidAnchoredCommitSHA(commit) {
			return nil, fmt.Errorf("%w: summaries are read by full commit numbers", domain.ErrValidation)
		}
	}
	listed, err := repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSummary, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return nil, err
		}
		summaries, err := s.anchoredSummaryStore()
		if err != nil {
			return nil, err
		}
		stored, err := summaries.ListCommitAnchoredSummaries(bound, repo, commits)
		if err != nil {
			return nil, err
		}
		return reader.summaries(bound, repo, stored)
	})
	if err != nil {
		return nil, err
	}
	s.nameAnchoredSummaries(ctx, listed)
	return listed, nil
}

// summaries keeps of the stored summaries those the reader may read, and
// says for each which segment it belongs to and whose session it is.
func (r anchoredReader) summaries(ctx context.Context, repo domain.ContentHash, stored []domain.AnchoredSummary) ([]domain.AnchoredSummary, error) {
	readable := []domain.AnchoredSummary{}
	views := map[anchoredKey]*domain.AnchoredSession{}
	for _, summary := range stored {
		key := anchoredKey{summary.Provider, summary.SessionID}
		view, seen := views[key]
		if !seen {
			session, err := r.store.GetAnchoredSession(ctx, repo, summary.Provider, summary.SessionID)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			if err == nil {
				given, ok, err := r.view(ctx, repo, session)
				if err != nil {
					return nil, err
				}
				if ok {
					given.Uploader = session.Uploader
					view = &given
				}
			}
			views[key] = view
		}
		if view == nil {
			continue
		}
		segment := view.Body.SegmentOfCommit(summary.Commit)
		if segment == 0 {
			continue
		}
		summary.SegmentNo, summary.Uploader = segment, view.Uploader
		readable = append(readable, summary)
	}
	return readable, nil
}

// nameAnchoredSummaries gives each summary its uploader's public handle.
func (s *Service) nameAnchoredSummaries(ctx context.Context, summaries []domain.AnchoredSummary) {
	accounts := map[string]struct{}{}
	for _, summary := range summaries {
		accounts[summary.Uploader] = struct{}{}
	}
	names := s.anchoredAccountNames(ctx, accounts)
	for index := range summaries {
		summaries[index].UploaderName = names[summaries[index].Uploader]
	}
}

var _ inbound.AnchoredSummaries = (*Service)(nil)
