package documentquery

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewSource struct {
	binding                domain.DocReadGenerationBinding
	envelope               domain.CIREnvelope
	total                  int
	root                   bool
	streamBytes            int64
	byteStorage            domain.DocRangeByteStorage
	chunk                  func(int) (domain.ConversationManifestChunk, error)
	block                  func(int) (domain.ContentHash, error)
	entry                  func(int) (domain.DocRangeEntry, error)
	location               func(int) (domain.DocEventIndex, error)
	read                   func(int, int) ([]byte, error)
	findFields             func(domain.DocEventIndex) (domain.DocFindFields, error)
	displayGeneration      func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error)
	displayPage            func(domain.DocEventIndex, int) (domain.DocDisplayPage, error)
	eventDisplayGeneration func(domain.DocEventIndex) (domain.DocDisplayProjectionGeneration, error)
	eventDisplayPage       func(domain.DocEventIndex, int) (domain.DocDisplayPage, error)
}

func (queries *Queries) docViewSource(ctx context.Context, repo, hash domain.ContentHash, legacyCompatibility bool) (docViewSource, error) {
	snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docViewSource{}, err
	}
	if err != nil {
		return queries.docViewSnapshotSource(ctx, repo, hash, nil, legacyCompatibility)
	}
	return queries.docViewSnapshotSource(ctx, repo, hash, &snapshot, legacyCompatibility)
}

func (queries *Queries) docViewSnapshotSource(ctx context.Context, repo, hash domain.ContentHash, snapshot *domain.Snapshot, legacyCompatibility bool) (docViewSource, error) {
	if snapshot != nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, *snapshot); err != nil {
			return docViewSource{}, err
		}
	}
	if snapshot != nil && snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
		storage := queries.view.Range
		if storage == nil {
			return docViewSource{}, domain.ErrDocRangeIndexUnavailable
		}
		chunks := queries.view.Chunks
		if chunks == nil {
			return docViewSource{}, domain.ErrUnsupportedDocumentIdentity
		}
		index, err := openDocViewRange(ctx, storage, queries.view.RangeSnapshots, queries.view.RangeGenerations, repo, hash)
		if err != nil {
			return docViewSource{}, err
		}
		var envelope domain.CIREnvelope
		if json.Unmarshal(index.envelope, &envelope) != nil {
			return docViewSource{}, domain.ErrIntegrity
		}
		cachedPage := -1
		var entries []domain.DocRangeEntry
		entry := func(position int) (domain.DocRangeEntry, error) {
			if position < 0 || position >= index.total {
				return domain.DocRangeEntry{}, domain.ErrValidation
			}
			page := position / domain.DocRangePageEvents
			if page != cachedPage {
				var err error
				entries, err = index.readPage(page)
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
		read := docChunkRangeReader(ctx, index.streamBytes, index.chunk, func(read context.Context, _ int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
			return chunks.ReadConversationChunk(read, repo, descriptor.Hash, descriptor.Bytes)
		})
		location := func(position int) (domain.DocEventIndex, error) {
			item, err := entry(position)
			return item.DocEventIndex, err
		}
		return docViewSource{binding: index.binding, envelope: envelope, total: index.total, root: true, block: index.block, entry: entry, location: location, read: read}, nil
	}
	if storage := queries.view.RangeGenerations; storage != nil && storage.InReadOnlyTransaction(ctx) {
		if source, ready, err := queries.legacySnapshotGenerationSource(ctx, repo, hash, snapshot); ready || err != nil {
			if _, enabled := queries.docBytePreparation(); err == nil && ready && enabled && !legacyCompatibility && source.chunk == nil && source.streamBytes > 0 {
				return docViewSource{}, errDocViewGenerationNeedsPreparation
			}
			return source, err
		}
		if queries.usesLegacyRangeReadiness() && snapshot != nil && !legacyCompatibility {
			return docViewSource{}, errDocViewGenerationNeedsPreparation
		}
	}
	ctx, release, err := queries.acquireViewBulk(ctx)
	if err != nil {
		return docViewSource{}, err
	}
	defer release()
	index, err := queries.legacyDocReadIndex(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	return legacyDocViewSource(queries.indexedReadSource(ctx, repo, hash, index)), nil
}

func legacyDocViewSource(source docReadSource) docViewSource {
	cachedEvent := -1
	var cachedBody []byte
	var cachedEntry domain.DocRangeEntry
	entry := func(position int) (domain.DocRangeEntry, error) {
		if position < 0 || position >= len(source.index.Events) {
			return domain.DocRangeEntry{}, domain.ErrValidation
		}
		if position == cachedEvent {
			return cachedEntry, nil
		}
		item := source.index.Events[position]
		raw, err := source.read(item.Offset, item.Length)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		metadata, err := domain.DecodeDocRangeMetadata(raw, item)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		cachedEvent, cachedBody, cachedEntry = position, raw, metadata
		return metadata, nil
	}
	location := func(position int) (domain.DocEventIndex, error) {
		if position < 0 || position >= len(source.index.Events) {
			return domain.DocEventIndex{}, domain.ErrValidation
		}
		return source.index.Events[position], nil
	}
	read := func(offset, length int) ([]byte, error) {
		if cachedEvent >= 0 && offset >= cachedEntry.Offset && length > 0 && length <= cachedEntry.Length && offset-cachedEntry.Offset <= cachedEntry.Length-length {
			return append([]byte(nil), cachedBody[offset-cachedEntry.Offset:offset-cachedEntry.Offset+length]...), nil
		}
		return source.read(offset, length)
	}
	fields := func(item domain.DocEventIndex) (domain.DocFindFields, error) {
		metadata, err := entry(item.Index)
		if err != nil {
			return domain.DocFindFields{}, err
		}
		if metadata.DocEventIndex != item {
			return domain.DocFindFields{}, domain.ErrIntegrity
		}
		return domain.BuildDocFindFields(item.Hash, cachedBody)
	}
	return docViewSource{envelope: source.index.Envelope, total: len(source.index.Events), entry: entry, location: location, read: read, findFields: fields}
}

func (queries *Queries) legacyGenerationSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, bool, error) {
	if queries.view.RangeGenerations == nil {
		return docViewSource{}, false, nil
	}
	snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docViewSource{}, false, err
	}
	if err != nil {
		return queries.legacySnapshotGenerationSource(ctx, repo, hash, nil)
	}
	return queries.legacySnapshotGenerationSource(ctx, repo, hash, &snapshot)
}

func (queries *Queries) legacySnapshotGenerationSource(ctx context.Context, repo, hash domain.ContentHash, snapshot *domain.Snapshot) (docViewSource, bool, error) {
	storage := queries.view.RangeGenerations
	if storage == nil {
		return docViewSource{}, false, nil
	}
	if snapshot != nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, *snapshot); err != nil {
			return docViewSource{}, false, err
		}
		if snapshot.RepoID != repo {
			return docViewSource{}, false, domain.ErrIntegrity
		}
		if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
			return docViewSource{}, false, nil
		}
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return docViewSource{}, false, domain.ErrConflict
	}
	source, err := openLegacyDocRangeMetadataSource(ctx, storage, repo, hash)
	if errors.Is(err, errDocViewGenerationNeedsPreparation) {
		return docViewSource{}, false, nil
	}
	if err != nil {
		return docViewSource{}, false, err
	}
	var read func(int, int) ([]byte, error)
	source.read = func(offset, length int) ([]byte, error) {
		bound, release, err := queries.acquireViewBulk(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		if read == nil {
			switch source.byteStorage {
			case domain.DocRangeByteStorageOriginal:
				chunks := queries.view.Chunks
				if chunks == nil {
					return nil, domain.ErrUnsupportedDocumentIdentity
				}
				read = docChunkRangeReader(outbound.WithDocReadOnly(bound), source.streamBytes, source.chunk, func(current context.Context, _ int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
					return chunks.ReadConversationChunk(current, repo, descriptor.Hash, descriptor.Bytes)
				})
			case domain.DocRangeByteStorageDerived:
				chunks := queries.view.DerivedChunks
				if chunks == nil {
					return nil, domain.ErrUnsupportedDocumentIdentity
				}
				read = docChunkRangeReader(outbound.WithDocReadOnly(bound), source.streamBytes, source.chunk, func(current context.Context, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
					return chunks.ReadLegacyDerivedByteChunk(current, repo, hash, source.binding, ordinal, descriptor)
				})
			case domain.DocRangeByteStorageNone:
				read, err = queries.eventRangeReader(outbound.WithDocReadOnly(bound), repo, hash)
				if err != nil {
					return nil, err
				}
			default:
				return nil, domain.ErrIntegrity
			}
		}
		return read(offset, length)
	}
	cachedEvent := -1
	var cachedFields domain.DocFindFields
	source.findFields = func(entry domain.DocEventIndex) (domain.DocFindFields, error) {
		current, err := source.location(entry.Index)
		if err != nil {
			return domain.DocFindFields{}, err
		}
		if current != entry {
			return domain.DocFindFields{}, domain.ErrIntegrity
		}
		if cachedEvent == entry.Index {
			return cachedFields, nil
		}
		fields, err := domain.BuildDocFindFieldsReader(ctx, entry.Hash, entry.Length, &docFindEventReader{source: source, entry: entry})
		if err != nil {
			return domain.DocFindFields{}, err
		}
		cachedEvent, cachedFields = entry.Index, fields
		return fields, nil
	}
	return source, true, nil
}
