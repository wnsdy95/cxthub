package documentquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docReadSource struct {
	index domain.DocReadIndex
	read  func(int, int) ([]byte, error)
}

func resourceReadForRepo[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	return readDocument(ctx, queries, repo, read, false)
}
func repositoryReadForRepo[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	return readDocument(ctx, queries, repo, read, true)
}

func (queries *Queries) legacyDocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if indexed := queries.index; indexed != nil {
		return indexed.DocReadIndex(ctx, repo, hash)
	}
	doc, err := queries.legacy.GetDoc(ctx, repo, hash)
	if err != nil {
		return domain.DocReadIndex{}, err
	}
	return domain.BuildDocReadIndex(doc)
}

func (queries *Queries) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	ctx = queries.readContext(ctx)
	return resourceReadForRepo(ctx, queries, repo, func(ctx context.Context) ([]domain.DocEventIndex, error) {
		query = strings.ToLower(strings.TrimSpace(query))
		if len([]rune(query)) < 2 || len([]rune(query)) > 256 || after < -1 || limit < 1 || limit > 201 {
			return nil, fmt.Errorf("%w: invalid event search", domain.ErrValidation)
		}
		if indexed := queries.index; indexed != nil {
			return indexed.SearchDocEvents(ctx, repo, hash, query, after, limit)
		}
		idx, err := queries.legacyDocReadIndex(ctx, repo, hash)
		if err != nil {
			return nil, err
		}
		return searchReadIndex(ctx, idx, query, after, limit)
	})
}

func searchReadIndex(ctx context.Context, idx domain.DocReadIndex, query string, after, limit int) ([]domain.DocEventIndex, error) {
	out := []domain.DocEventIndex{}
	for _, e := range idx.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Index > after && e.Text != "" && strings.Contains(strings.ToLower(e.Text), query) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (queries *Queries) eventRangeReader(ctx context.Context, repo, hash domain.ContentHash) (func(int, int) ([]byte, error), error) {
	chunks := queries.legacy
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
		for chunkIndex := start / domain.ChunkTarget; chunkIndex <= (end-1)/domain.ChunkTarget; chunkIndex++ {
			if chunkIndex >= len(man.Chunks) {
				return nil, domain.ErrIntegrity
			}
			if cached != chunkIndex {
				nextBody, err := chunks.GetChunk(ctx, repo, man.Chunks[chunkIndex])
				if err != nil {
					return nil, err
				}
				if domain.HashContent(nextBody) != man.Chunks[chunkIndex] {
					return nil, domain.ErrIntegrity
				}
				cached, cachedBody = chunkIndex, nextBody
			}
			body := cachedBody
			lo := max(start-chunkIndex*domain.ChunkTarget, 0)
			hi := min(end-chunkIndex*domain.ChunkTarget, len(body))
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

func (queries *Queries) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	ctx = queries.readContext(ctx)
	return resourceReadForRepo(ctx, queries, repo, func(ctx context.Context) (domain.DocFragmentPage, error) {
		out := domain.DocFragmentPage{Fragments: []domain.DocEventFragment{}}
		if index < 0 || offset < 0 || limit < 1 || limit > 50 || budget < 4 || budget > 64<<10 {
			return out, domain.ErrValidation
		}
		source, err := queries.documentReadSource(ctx, repo, hash)
		if err != nil {
			return out, err
		}
		idx := source.index
		out.Total = len(idx.Events)
		out.NextIndex = index
		out.NextOffset = offset
		if index > out.Total || (index == out.Total && offset != 0) {
			return out, domain.ErrValidation
		}
		if index == out.Total {
			return out, nil
		}
		read := source.read
		for index < out.Total && len(out.Fragments) < limit && budget >= 4 {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			item := idx.Events[index]
			if offset >= item.Length {
				return out, domain.ErrValidation
			}
			readLength := min(item.Length-offset, budget)
			raw, err := read(item.Offset+offset, readLength)
			if err != nil {
				return out, err
			}
			if len(raw) == 0 || !utf8.RuneStart(raw[0]) {
				return out, domain.ErrValidation
			}
			if offset+readLength < item.Length {
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
	})
}

func (queries *Queries) documentReadSource(ctx context.Context, repo, hash domain.ContentHash) (docReadSource, error) {
	index, err := queries.legacyDocReadIndex(ctx, repo, hash)
	if err != nil {
		return docReadSource{}, err
	}
	return queries.indexedReadSource(ctx, repo, hash, index), nil
}

func (queries *Queries) indexedReadSource(ctx context.Context, repo, hash domain.ContentHash, index domain.DocReadIndex) docReadSource {
	var read func(int, int) ([]byte, error)
	return docReadSource{index: index, read: func(start, length int) ([]byte, error) {
		if read == nil {
			var err error
			read, err = queries.eventRangeReader(ctx, repo, hash)
			if err != nil {
				return nil, err
			}
		}
		return read(start, length)
	}}
}
