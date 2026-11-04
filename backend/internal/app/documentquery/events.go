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

type docReadProjection uint8

const (
	docReadMetadata docReadProjection = iota
	docReadRanges
	docReadSearch
)

func resourceReadForRepo[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	return readDocument(ctx, queries, repo, read, false)
}
func repositoryReadForRepo[Result any](ctx context.Context, queries *Queries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	return readDocument(ctx, queries, repo, read, true)
}

func (queries *Queries) docReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	source, err := queries.documentReadSource(ctx, repo, hash)
	return source.index, err
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

func (queries *Queries) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	ctx = queries.readContext(ctx)
	return resourceReadForRepo(ctx, queries, repo, func(ctx context.Context) (domain.DocEventPage, error) {
		out := domain.DocEventPage{Hash: hash, Next: -1, Events: []domain.CIREvent{}}
		if offset < -1 || limit < 0 || limit > 100 {
			return out, fmt.Errorf("%w: invalid event range", domain.ErrValidation)
		}
		if limit == 0 {
			limit = 50
		}
		var previous []domain.ContentHash
		if base != "" {
			var err error
			previous, err = queries.docEventBaseHashes(ctx, repo, base)
			if err != nil {
				return out, err
			}
		}
		projection, err := queries.rootEventProjection(ctx, repo, hash, domain.RootEventSelection{Offset: offset, Limit: limit, Base: previous})
		if err != nil {
			return out, err
		}
		if projection != nil {
			return projection.Page(ctx)
		}
		source, err := queries.documentReadSource(ctx, repo, hash)
		if err != nil {
			return out, err
		}
		idx := source.index
		out.Envelope = idx.Envelope
		out.Total = len(idx.Events)
		if base != "" {
			for out.Inherited < len(idx.Events) && out.Inherited < len(previous) && idx.Events[out.Inherited].Hash == previous[out.Inherited] {
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
		read := source.read
		bytes := 0
		for eventIndex := offset; eventIndex < out.Total && len(out.Events) < limit; eventIndex++ {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			item := idx.Events[eventIndex]
			if len(out.Events) > 0 && bytes+item.Length > 512<<10 {
				out.Next = eventIndex
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
	})
}

func (queries *Queries) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	ctx = queries.readContext(ctx)
	return resourceReadForRepo(ctx, queries, repo, func(ctx context.Context) ([]domain.DocEventIndex, error) {
		query = strings.ToLower(strings.TrimSpace(query))
		if len([]rune(query)) < 2 || len([]rune(query)) > 256 || after < -1 || limit < 1 || limit > 201 {
			return nil, fmt.Errorf("%w: invalid event search", domain.ErrValidation)
		}
		root, err := queries.rootReadSource(ctx, repo, hash, docReadSearch)
		if err != nil {
			return nil, err
		}
		if root != nil {
			return searchReadIndex(ctx, root.index, query, after, limit)
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

func (queries *Queries) rootReadSource(ctx context.Context, repo, hash domain.ContentHash, projection docReadProjection) (*docReadSource, error) {
	return repositoryReadForRepo(ctx, queries, repo, func(bound context.Context) (*docReadSource, error) {
		snapshot, err := queries.metadata.GetSnapshot(bound, repo, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return nil, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
			return nil, nil
		}
		source, err := queries.verifiedRootReadSource(bound, repo, snapshot, projection)
		return &source, err
	})
}

func matchReadSnapshot(expected domain.DocumentRef, snapshot domain.Snapshot) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := snapshot.DocumentRef().Validate(); err != nil {
		return err
	}
	if expected.Identity == domain.DocumentIdentityLegacy && snapshot.DocIdentity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if !expected.MatchesSnapshot(snapshot) {
		return domain.ErrIntegrity
	}
	return nil
}

func (queries *Queries) verifiedRootDoc(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) (domain.VerifiedSessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.VerifiedSessionDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	reader := queries.verified
	if reader == nil {
		return domain.VerifiedSessionDoc{}, domain.ErrUnsupportedDocumentIdentity
	}
	doc, err := reader.ReadVerifiedDoc(ctx, repo, snapshot.DocHash)
	if err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	if !doc.Reference().Matches(snapshot) {
		return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
	}
	return doc, ctx.Err()
}

func (queries *Queries) verifiedRootReadSource(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot, projection docReadProjection) (docReadSource, error) {
	doc, err := queries.verifiedRootDoc(ctx, repo, snapshot)
	if err != nil {
		return docReadSource{}, err
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return docReadSource{}, err
	}
	var index domain.DocReadIndex
	switch projection {
	case docReadRanges:
		index, err = plan.BuildRangesContext(ctx)
	case docReadMetadata:
		index, err = plan.BuildMetadataContext(ctx)
	case docReadSearch:
		index, err = plan.BuildContext(ctx, nil)
	default:
		return docReadSource{}, domain.ErrIntegrity
	}
	if err != nil {
		return docReadSource{}, err
	}
	if err := ctx.Err(); err != nil {
		return docReadSource{}, err
	}
	return docReadSource{index: index, read: func(start, length int) ([]byte, error) { return doc.EventStreamRange(ctx, start, length) }}, nil
}

func (queries *Queries) documentReadSource(ctx context.Context, repo, hash domain.ContentHash) (docReadSource, error) {
	root, err := queries.rootReadSource(ctx, repo, hash, docReadRanges)
	if err != nil {
		return docReadSource{}, err
	}
	if root != nil {
		return *root, nil
	}
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

func (queries *Queries) rootEventProjection(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (*domain.VerifiedRootEvents, error) {
	reader := queries.rootEvents
	if reader == nil {
		return nil, nil
	}
	snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return nil, err
	}
	if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
		return nil, nil
	}
	projection, err := reader.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if err != nil {
		return nil, err
	}
	if !projection.Reference().Matches(snapshot) {
		return nil, domain.ErrIntegrity
	}
	return &projection, ctx.Err()
}

func (queries *Queries) docEventBaseHashes(ctx context.Context, repo, hash domain.ContentHash) ([]domain.ContentHash, error) {
	projection, err := queries.rootEventProjection(ctx, repo, hash, domain.RootEventSelection{HashesOnly: true})
	if err != nil {
		return nil, err
	}
	if projection != nil {
		return projection.EventHashes()
	}
	index, err := queries.docReadIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	hashes := make([]domain.ContentHash, len(index.Events))
	for position, event := range index.Events {
		hashes[position] = event.Hash
	}
	return hashes, ctx.Err()
}
