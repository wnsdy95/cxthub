package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docViewBytes = 128 << 10
const docViewScanEvents = 512

type docViewSource struct {
	envelope domain.CIREnvelope
	total    int
	blocks   []domain.ContentHash
	entry    func(int) (domain.DocRangeEntry, error)
	location func(int) (domain.DocEventIndex, error)
	read     func(int, int) ([]byte, error)
}

func (service *Service) prepareDocView(ctx context.Context, repo, hash domain.ContentHash) error {
	needs, err := repositoryReadForRepo(ctx, service, repo, func(read context.Context) (bool, error) {
		snapshot, err := service.meta.GetSnapshot(read, repo, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return false, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
			return false, nil
		}
		storage, ok := service.blobs.(outbound.DocRangeStore)
		if !ok {
			return false, domain.ErrDocRangeIndexUnavailable
		}
		_, err = storage.DocRangeIndex(read, repo, hash)
		if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			return true, nil
		}
		return false, err
	})
	if err != nil || !needs {
		return err
	}
	return resourceWork(ctx, ResourceBulk, func(admitted context.Context) error {
		return service.blobs.(outbound.DocRangeStore).EnsureDocRangeIndex(admitted, repo, hash)
	})
}

func (service *Service) docViewSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docViewSource{}, err
	}
	if err == nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return docViewSource{}, err
		}
	}
	if err == nil && snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
		storage, ok := service.blobs.(outbound.DocRangeStore)
		if !ok {
			return docViewSource{}, domain.ErrDocRangeIndexUnavailable
		}
		chunks, ok := service.blobs.(outbound.ConversationChunkReader)
		if !ok {
			return docViewSource{}, domain.ErrUnsupportedDocumentIdentity
		}
		index, err := storage.DocRangeIndex(ctx, repo, hash)
		if err != nil {
			return docViewSource{}, err
		}
		if err := index.Validate(hash); err != nil {
			return docViewSource{}, err
		}
		var envelope domain.CIREnvelope
		if json.Unmarshal(index.Manifest.Envelope, &envelope) != nil {
			return docViewSource{}, domain.ErrIntegrity
		}
		cachedPage := -1
		var entries []domain.DocRangeEntry
		entry := func(position int) (domain.DocRangeEntry, error) {
			if position < 0 || position >= int(index.Manifest.EventCount) {
				return domain.DocRangeEntry{}, domain.ErrValidation
			}
			page := position / domain.DocRangePageEvents
			if page != cachedPage {
				var err error
				entries, err = storage.DocRangePage(ctx, repo, index, page)
				if err != nil {
					return domain.DocRangeEntry{}, err
				}
				cachedPage = page
			}
			if len(entries) <= position%domain.DocRangePageEvents {
				return domain.DocRangeEntry{}, domain.ErrIntegrity
			}
			return entries[position%domain.DocRangePageEvents], nil
		}
		cachedChunk := -1
		var body []byte
		read := func(offset, length int) ([]byte, error) {
			if offset < 0 || length <= 0 || length > docViewBytes || int64(offset) > index.Manifest.StreamBytes-int64(length) {
				return nil, domain.ErrValidation
			}
			result := make([]byte, 0, length)
			for position := offset; position < offset+length; {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				chunk := position / domain.ConversationManifestChunkBytes
				if chunk >= len(index.Manifest.Chunks) {
					return nil, domain.ErrIntegrity
				}
				descriptor := index.Manifest.Chunks[chunk]
				if cachedChunk != chunk {
					var err error
					body, err = chunks.ReadConversationChunk(ctx, repo, descriptor.Hash, descriptor.Bytes)
					if err != nil {
						return nil, err
					}
					if int64(len(body)) != descriptor.Bytes || domain.HashContent(body) != descriptor.Hash {
						return nil, domain.ErrIntegrity
					}
					cachedChunk = chunk
				}
				start := position % domain.ConversationManifestChunkBytes
				count := min(offset+length-position, len(body)-start)
				if count <= 0 {
					return nil, domain.ErrIntegrity
				}
				result = append(result, body[start:start+count]...)
				position += count
			}
			return result, nil
		}
		location := func(position int) (domain.DocEventIndex, error) {
			item, err := entry(position)
			return item.DocEventIndex, err
		}
		return docViewSource{envelope: envelope, total: int(index.Manifest.EventCount), blocks: index.Pages, entry: entry, location: location, read: read}, nil
	}
	ctx, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return docViewSource{}, err
	}
	defer release()
	source, err := service.documentReadSource(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	entry := func(position int) (domain.DocRangeEntry, error) {
		item := source.index.Events[position]
		raw, err := source.read(item.Offset, item.Length)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		event, err := domain.DecodeIndexedEvent(raw, item)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		return domain.DocRangeEntry{DocEventIndex: item, Kind: event.Kind, ToolName: event.ToolName, CompactSummary: event.CompactSummary}, nil
	}
	location := func(position int) (domain.DocEventIndex, error) {
		if position < 0 || position >= len(source.index.Events) {
			return domain.DocEventIndex{}, domain.ErrValidation
		}
		return source.index.Events[position], nil
	}
	return docViewSource{envelope: source.index.Envelope, total: len(source.index.Events), entry: entry, location: location, read: source.read}, nil
}

func docViewInherited(ctx context.Context, source, base docViewSource) (int, error) {
	inherited, total := 0, min(source.total, base.total)
	for inherited < total {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		page := inherited / domain.DocRangePageEvents
		if inherited%domain.DocRangePageEvents == 0 && page < len(source.blocks) && page < len(base.blocks) && source.blocks[page] == base.blocks[page] {
			inherited = min(total, inherited+domain.DocRangePageEvents)
			continue
		}
		left, err := source.location(inherited)
		if err != nil {
			return 0, err
		}
		right, err := base.location(inherited)
		if err != nil {
			return 0, err
		}
		if left.Hash != right.Hash {
			break
		}
		inherited++
	}
	return inherited, nil
}

func (service *Service) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	if offset < -1 || limit < 0 || limit > 100 || (view != "chat" && view != "prompts" && view != "all" && view != "raw") {
		return domain.DocViewPage{}, domain.ErrValidation
	}
	if limit == 0 {
		limit = 20
	}
	if err := service.prepareDocView(ctx, repo, hash); err != nil {
		return domain.DocViewPage{}, err
	}
	if base != "" {
		if err := service.prepareDocView(ctx, repo, base); err != nil {
			return domain.DocViewPage{}, err
		}
	}
	ctx, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		return domain.DocViewPage{}, err
	}
	defer release()
	return repositoryReadForRepo(ctx, service, repo, func(read context.Context) (domain.DocViewPage, error) {
		out := domain.DocViewPage{Hash: hash, Next: -1, Events: []domain.DocViewEvent{}}
		source, err := service.docViewSource(read, repo, hash)
		if err != nil {
			return out, err
		}
		out.Envelope, out.Total = source.envelope, source.total
		if base != "" {
			previous, err := service.docViewSource(read, repo, base)
			if err != nil {
				return out, err
			}
			out.Inherited, err = docViewInherited(read, source, previous)
			if err != nil {
				return out, err
			}
		}
		if offset == -1 {
			offset = out.Inherited
		}
		if offset > source.total {
			return out, domain.ErrValidation
		}
		out.Offset = offset
		visible, bytes, position := 0, 0, offset
		for position < source.total && position-offset < docViewScanEvents && visible < limit {
			if err := read.Err(); err != nil {
				return out, err
			}
			entry, err := source.entry(position)
			if err != nil {
				return out, err
			}
			shown := view == "all" || view == "raw" || (entry.Kind == domain.EventMessage && (view == "chat" || (entry.Role == string(domain.RoleUser) && !entry.CompactSummary)))
			deferred := entry.Length > docViewBytes || (view != "raw" && (!shown || entry.Kind == domain.EventToolCall || entry.Kind == domain.EventToolResult || entry.Kind == domain.EventReasoning || entry.CompactSummary))
			event := domain.CIREvent{Kind: entry.Kind, Role: domain.Role(entry.Role), Seq: entry.Seq, ToolName: entry.ToolName, CompactSummary: entry.CompactSummary}
			cost := entry.Length + 128
			if deferred {
				metadata, err := json.Marshal(domain.DocViewEvent{Event: event, Index: position, Deferred: true, Bytes: entry.Length})
				if err != nil {
					return out, err
				}
				cost = len(metadata) + 1
			}
			if len(out.Events) > 0 && bytes+cost > docViewBytes {
				break
			}
			if !deferred {
				raw, err := source.read(entry.Offset, entry.Length)
				if err != nil {
					return out, err
				}
				event, err = domain.DecodeIndexedEvent(raw, entry.DocEventIndex)
				if err != nil {
					return out, err
				}
			}
			out.Events = append(out.Events, domain.DocViewEvent{Event: event, Index: position, Deferred: deferred, Bytes: entry.Length})
			bytes += cost
			if shown {
				visible++
			}
			position++
		}
		if position < source.total {
			out.Next = position
		}
		return out, nil
	})
}

func (service *Service) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	if index < 0 || offset < 0 || budget < 4 || budget > 64<<10 {
		return domain.DocViewFragment{}, domain.ErrValidation
	}
	if err := service.prepareDocView(ctx, repo, hash); err != nil {
		return domain.DocViewFragment{}, err
	}
	ctx, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	defer release()
	return repositoryReadForRepo(ctx, service, repo, func(read context.Context) (domain.DocViewFragment, error) {
		source, err := service.docViewSource(read, repo, hash)
		if err != nil {
			return domain.DocViewFragment{}, err
		}
		if index >= source.total {
			return domain.DocViewFragment{}, domain.ErrValidation
		}
		entry, err := source.location(index)
		if err != nil {
			return domain.DocViewFragment{}, err
		}
		if offset >= entry.Length {
			return domain.DocViewFragment{}, domain.ErrValidation
		}
		raw, err := source.read(entry.Offset+offset, min(budget, entry.Length-offset))
		if err != nil {
			return domain.DocViewFragment{}, err
		}
		if len(raw) == 0 || !utf8.RuneStart(raw[0]) {
			return domain.DocViewFragment{}, domain.ErrValidation
		}
		for len(raw) > 0 && !utf8.Valid(raw) {
			raw = raw[:len(raw)-1]
		}
		if len(raw) == 0 {
			return domain.DocViewFragment{}, fmt.Errorf("%w: invalid event UTF-8", domain.ErrIntegrity)
		}
		next := offset + len(raw)
		if next == entry.Length {
			next = -1
		}
		return domain.DocViewFragment{Index: index, Offset: offset, Next: next, Total: entry.Length, JSON: string(raw)}, nil
	})
}
