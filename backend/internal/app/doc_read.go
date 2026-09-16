package app

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) docReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
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
	out := domain.DocEventPage{Hash: hash, Next: -1, Events: []domain.CIREvent{}}
	if offset < -1 || limit < 0 || limit > 100 {
		return out, fmt.Errorf("%w: invalid event range", domain.ErrValidation)
	}
	if limit == 0 {
		limit = 50
	}
	idx, err := s.docReadIndex(ctx, repo, hash)
	if err != nil {
		return out, err
	}
	out.Envelope = idx.Envelope
	out.Total = len(idx.Events)
	if base != "" {
		previous, err := s.docReadIndex(ctx, repo, base)
		if err != nil {
			return out, err
		}
		for out.Inherited < len(idx.Events) && out.Inherited < len(previous.Events) && idx.Events[out.Inherited].Hash == previous.Events[out.Inherited].Hash {
			out.Inherited++
		}
	}
	if offset == -1 {
		offset = out.Inherited
	}
	if offset > out.Total {
		return out, fmt.Errorf("%w: event offset exceeds document", domain.ErrValidation)
	}
	out.Offset = offset
	if offset == out.Total {
		return out, nil
	}
	read, err := s.eventRangeReader(ctx, repo, hash)
	if err != nil {
		return out, err
	}
	bytes := 0
	for i := offset; i < out.Total && len(out.Events) < limit; i++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		item := idx.Events[i]
		if len(out.Events) > 0 && bytes+item.Length > 512<<10 {
			out.Next = i
			return out, nil
		}
		raw, err := read(item.Offset, item.Length)
		if err != nil {
			return out, err
		}
		ev, err := domain.DecodeIndexedEvent(raw, item)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, ev)
		bytes += item.Length
	}
	if next := offset + len(out.Events); next < out.Total {
		out.Next = next
	}
	return out, nil
}

func (s *Service) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, q string, after, limit int) ([]domain.DocEventIndex, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if len([]rune(q)) < 2 || len([]rune(q)) > 256 || after < -1 || limit < 1 || limit > 201 {
		return nil, fmt.Errorf("%w: invalid event search", domain.ErrValidation)
	}
	if indexed, ok := s.blobs.(outbound.DocReadStore); ok {
		return indexed.SearchDocEvents(ctx, repo, hash, q, after, limit)
	}
	idx, err := s.docReadIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	out := []domain.DocEventIndex{}
	for _, e := range idx.Events {
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
// non-nil set proves no readable event in this repository matches the literal.
func (s *Service) MatchingDocHashes(ctx context.Context, repo domain.ContentHash, q string) (map[domain.ContentHash]bool, error) {
	if indexed, ok := s.blobs.(interface {
		MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error)
	}); ok {
		return indexed.MatchingDocHashes(ctx, repo, strings.ToLower(strings.TrimSpace(q)))
	}
	return nil, nil
}

func (s *Service) eventRangeReader(ctx context.Context, repo, hash domain.ContentHash) (func(int, int) ([]byte, error), error) {
	chunks := s.blobs
	man, err := chunks.GetDocManifest(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if man.Format != domain.ChunkFormatV2 {
		return nil, fmt.Errorf("%w: indexed document requires v2 chunks", domain.ErrIntegrity)
	}
	cache := map[int][]byte{}
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
			body, yes := cache[j]
			if !yes {
				body, err = chunks.GetChunk(ctx, repo, man.Chunks[j])
				if err != nil {
					return nil, err
				}
				if domain.HashContent(body) != man.Chunks[j] {
					return nil, domain.ErrIntegrity
				}
				cache[j] = body
			}
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

// ReadDocFragments caps I/O and response text independently of event size.
// Full-event verification occurs when building the index; every range is read
// from hash-verified manifest chunks, preserving UTF-8 and exact JSON bytes.
func (s *Service) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	out := domain.DocFragmentPage{Fragments: []domain.DocEventFragment{}}
	if index < 0 || offset < 0 || limit < 1 || limit > 50 || budget < 4 || budget > 64<<10 {
		return out, domain.ErrValidation
	}
	idx, err := s.docReadIndex(ctx, repo, hash)
	if err != nil {
		return out, err
	}
	out.Total = len(idx.Events)
	out.NextIndex = index
	out.NextOffset = offset
	if index > out.Total || (index == out.Total && offset != 0) {
		return out, domain.ErrValidation
	}
	if index == out.Total {
		return out, nil
	}
	read, err := s.eventRangeReader(ctx, repo, hash)
	if err != nil {
		return out, err
	}
	for index < out.Total && len(out.Fragments) < limit && budget >= 4 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		item := idx.Events[index]
		if offset >= item.Length {
			return out, domain.ErrValidation
		}
		n := min(item.Length-offset, budget)
		raw, err := read(item.Offset+offset, n)
		if err != nil {
			return out, err
		}
		if len(raw) == 0 || !utf8.RuneStart(raw[0]) {
			return out, domain.ErrValidation
		}
		if offset+n < item.Length {
			for len(raw) > 0 && !utf8.Valid(raw) {
				raw = raw[:len(raw)-1]
			}
		}
		if len(raw) == 0 {
			return out, domain.ErrIntegrity
		}
		complete := offset+len(raw) == item.Length
		out.Fragments = append(out.Fragments, domain.DocEventFragment{Index: index, Offset: offset, Complete: complete, JSON: string(raw)})
		budget -= len(raw)
		offset += len(raw)
		if complete {
			index++
			offset = 0
		}
	}
	out.NextIndex = index
	out.NextOffset = offset
	return out, nil
}
