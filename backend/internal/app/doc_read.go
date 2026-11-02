package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) docReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	source, err := s.documentReadSource(ctx, repo, hash)
	return source.index, err
}

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

// ReadDocEvents reads a bounded window from verified chunks. A base hash only
// selects the exact inherited prefix; it never rewrites or truncates the archive.
func (s *Service) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	queries, err := s.DocumentEventQueries()
	if err != nil {
		return domain.DocEventPage{}, err
	}
	return queries.ReadDocEvents(ctx, repo, hash, base, offset, limit)
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

func (s *Service) eventRangeReader(ctx context.Context, repo, hash domain.ContentHash) (func(int, int) ([]byte, error), error) {
	chunks := s.blobs
	man, err := chunks.GetDocManifest(ctx, repo, hash)
	if errors.Is(err, domain.ErrAgentHistoryUnavailable) || errors.Is(err, domain.ErrNotFound) {
		doc, err := chunks.GetDoc(ctx, repo, hash)
		if err != nil {
			return nil, err
		}
		if doc.DocumentRef() != (domain.DocumentRef{Hash: hash}) {
			return nil, domain.ErrUnsupportedDocumentIdentity
		}
		verified, err := domain.VerifySessionDoc(doc)
		if err != nil {
			return nil, err
		}
		return func(offset, length int) ([]byte, error) { return verified.EventStreamRange(ctx, offset, length) }, nil
	}
	if err != nil {
		return nil, err
	}
	if man.Format != domain.ChunkFormatV2 {
		return nil, fmt.Errorf("%w: indexed document requires v2 chunks", domain.ErrIntegrity)
	}
	cached := -1
	var cachedBody []byte
	return func(start, length int) ([]byte, error) {
		if start < 0 || length <= 0 || start >= len(man.Chunks)*domain.ChunkTarget || length > len(man.Chunks)*domain.ChunkTarget-start {
			return nil, domain.ErrIntegrity
		}
		end := start + length
		raw := make([]byte, 0, length)
		for j := start / domain.ChunkTarget; j <= (end-1)/domain.ChunkTarget; j++ {
			if j >= len(man.Chunks) {
				return nil, domain.ErrIntegrity
			}
			if cached != j {
				nextBody, err := chunks.GetChunk(ctx, repo, man.Chunks[j])
				if err != nil {
					return nil, err
				}
				if domain.HashContent(nextBody) != man.Chunks[j] {
					return nil, domain.ErrIntegrity
				}
				cached, cachedBody = j, nextBody
			}
			body := cachedBody
			lo := max(start-j*domain.ChunkTarget, 0)
			hi := min(end-j*domain.ChunkTarget, len(body))
			if lo > hi {
				return nil, domain.ErrIntegrity
			}
			raw = append(raw, body[lo:hi]...)
		}
		if len(raw) != length {
			return nil, domain.ErrIntegrity
		}
		return raw, nil
	}, nil
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
