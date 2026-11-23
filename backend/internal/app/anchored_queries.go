package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// anchoredFragmentEvents is the most events one piece of a conversation holds.
const anchoredFragmentEvents = 50

// ReadAnchoredSessionFragments reads a session's conversation from one event
// on, as pieces of JSON within a byte budget, and only what the caller may
// read of it. Where the event asked for is not open, reading goes on at the
// next open part; a page never crosses into a part that is not open. The
// document is read with the server's own access once that is decided, as the
// search does; its hash is never answered.
func (s *Service) ReadAnchoredSessionFragments(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, index, offset, limit, budget int) (domain.AnchoredFragmentPage, error) {
	if index < 0 || offset < 0 || limit < 1 || limit > anchoredFragmentEvents || budget < 1 {
		return domain.AnchoredFragmentPage{}, fmt.Errorf("%w: invalid event range", domain.ErrValidation)
	}
	access, err := s.AnchoredSessionContentAccess(ctx, repo, provider, sessionID)
	if err != nil {
		return domain.AnchoredFragmentPage{}, err
	}
	page := domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{}, Total: access.Events, Open: access.Open, NextIndex: -1}
	start, span, open := access.From(index)
	if !open {
		return page, nil
	}
	if start != index {
		offset = 0
	}
	if limit > span[1]-start {
		limit = span[1] - start
	}
	read, err := s.ReadDocFragments(inbound.WithSystemActor(ctx), repo, access.Doc, start, offset, limit, budget)
	if err != nil {
		return domain.AnchoredFragmentPage{}, err
	}
	// What the reader returned is held to what was asked: from the start, in
	// order, and never past the end of the open span.
	last := start
	for _, fragment := range read.Fragments {
		if fragment.Index < last || fragment.Index >= span[1] || len(page.Fragments) >= limit+1 {
			break
		}
		last = fragment.Index
		page.Fragments = append(page.Fragments, fragment)
	}
	next, nextOffset := read.NextIndex, read.NextOffset
	if next < start || (next == start && nextOffset <= offset) {
		return domain.AnchoredFragmentPage{}, fmt.Errorf("%w: a conversation page did not advance", domain.ErrIntegrity)
	}
	if next >= span[1] {
		// The span is read; go on at the next open one, if any.
		next, nextOffset = span[1], 0
	}
	if following, _, more := access.From(next); more {
		if following != next {
			nextOffset = 0
		}
		page.NextIndex, page.NextOffset = following, nextOffset
	}
	return page, nil
}

// AnchoredReadService is the application service limited to the reads of the
// session structure. A process that must hold no command service, like the
// server AI tools call, is given this: it lists, opens and searches sessions
// and reads their conversations and summaries, and nothing it holds writes.
// Each read is answered under the rule every reader is answered under: all of
// a session to its uploader, the open parts to anyone else. Whether the
// caller may read the repository at all is decided before it is asked.
type AnchoredReadService struct{ service *Service }

// NewAnchoredReadService builds the reads over the stores. It fails when the
// stores do not keep sessions, their open parts or their summaries.
func NewAnchoredReadService(meta outbound.MetadataStore, blobs outbound.BlobStore, repositories outbound.RepositoryStore) (*AnchoredReadService, error) {
	if meta == nil || blobs == nil {
		return nil, fmt.Errorf("%w: session reads need the metadata and document stores", domain.ErrValidation)
	}
	service := &Service{meta: meta, blobs: blobs, repositories: repositories}
	if _, err := service.anchoredSessionStore(); err != nil {
		return nil, err
	}
	if _, err := service.anchoredPublicationStore(); err != nil {
		return nil, err
	}
	if _, err := service.anchoredSummaryStore(); err != nil {
		return nil, err
	}
	return &AnchoredReadService{service: service}, nil
}

var _ inbound.AnchoredQueries = (*AnchoredReadService)(nil)

func (q *AnchoredReadService) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	return q.service.ListRepos(ctx, team)
}

func (q *AnchoredReadService) GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error) {
	return q.service.GetAnchoredSession(ctx, repo, provider, sessionID)
}

func (q *AnchoredReadService) ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	return q.service.ListAnchoredSessions(ctx, repo)
}

func (q *AnchoredReadService) AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error) {
	return q.service.AnchoredCommitSegments(ctx, repo, commitSHA)
}

func (q *AnchoredReadService) ListAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	return q.service.ListAnchoredSummaries(ctx, repo, commits)
}

func (q *AnchoredReadService) ReadAnchoredMemory(ctx context.Context, repo domain.ContentHash, branch string, local []string, limit int) (domain.AnchoredMemory, error) {
	return q.service.ReadAnchoredMemory(ctx, repo, branch, local, limit)
}

func (q *AnchoredReadService) SearchAnchored(ctx context.Context, repo domain.ContentHash, query string) (domain.AnchoredSearchResult, error) {
	return q.service.SearchAnchored(ctx, repo, query)
}

func (q *AnchoredReadService) ReadAnchoredSessionFragments(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, index, offset, limit, budget int) (domain.AnchoredFragmentPage, error) {
	return q.service.ReadAnchoredSessionFragments(ctx, repo, provider, sessionID, index, offset, limit, budget)
}
