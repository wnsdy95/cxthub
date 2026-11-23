package app

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) legacyDocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if indexed, ok := s.blobs.(outbound.DocReadStore); ok {
		return indexed.DocReadIndex(ctx, repo, hash)
	}
	doc, err := s.blobs.GetDoc(ctx, repo, hash)
	if err != nil {
		return domain.DocReadIndex{}, err
	}
	return domain.BuildDocReadIndex(doc)
}

func (s *Service) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, q string, after, limit int) ([]domain.DocEventIndex, error) {
	queries, err := s.DocumentEventQueries()
	if err != nil {
		return nil, err
	}
	return queries.SearchDocEvents(ctx, repo, hash, q, after, limit)
}

func searchReadIndex(ctx context.Context, idx domain.DocReadIndex, q string, after, limit int) ([]domain.DocEventIndex, error) {
	out := []domain.DocEventIndex{}
	for _, e := range idx.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Index > after && e.Text != "" && strings.Contains(strings.ToLower(e.Text), q) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// A nil candidate set means the adapter searches each indexed document. An empty
// non-nil set excludes legacy indexed documents only; roots still require their
// explicit current-byte reader before searching the verified projection.
func (s *Service) MatchingDocHashes(ctx context.Context, repo domain.ContentHash, q string) (map[domain.ContentHash]bool, error) {
	return resourceReadForRepo(ctx, s, repo, func(ctx context.Context) (map[domain.ContentHash]bool, error) {
		if indexed, ok := s.blobs.(interface {
			MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error)
		}); ok {
			return indexed.MatchingDocHashes(ctx, repo, strings.ToLower(strings.TrimSpace(q)))
		}
		return nil, nil
	})
}

// ReadDocFragments caps response text independently of event size. Legacy
// indexes use bounded chunk ranges. Roots first verify the whole current source,
// then return ranges from those owned bytes, preserving UTF-8 and exact JSON.
func (s *Service) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	queries, err := s.DocumentEventQueries()
	if err != nil {
		return domain.DocFragmentPage{}, err
	}
	return queries.ReadDocFragments(ctx, repo, hash, index, offset, limit, budget)
}
