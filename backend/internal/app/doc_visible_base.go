package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) openDocVisibleBase(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return docViewSource{}, err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
		if err := service.checkVisibleRootSnapshot(hash, snapshot); err != nil {
			return docViewSource{}, err
		}
		return service.docViewSource(ctx, repo, hash, false)
	}
	storage, ok := service.blobs.(outbound.DocRangeGenerationStore)
	if !ok {
		return docViewSource{}, domain.ErrDocVisibleProjectionUnsupported
	}
	return openLegacyDocRangeMetadataSource(ctx, storage, repo, hash)
}

func openLegacyDocRangeMetadataSource(ctx context.Context, storage outbound.DocRangeGenerationStore, repo, hash domain.ContentHash) (docViewSource, error) {
	reader, err := storage.OpenDocRangeGeneration(ctx, repo, hash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return docViewSource{}, errDocViewGenerationNeedsPreparation
	}
	if err != nil {
		return docViewSource{}, err
	}
	generation, binding := reader.Descriptor(), reader.Binding()
	if generation.Validate(hash) != nil || generation.Identity() != domain.DocumentIdentityLegacy || binding.Validate() != nil {
		return docViewSource{}, domain.ErrIntegrity
	}
	raw, err := reader.Envelope(ctx)
	if err != nil {
		return docViewSource{}, err
	}
	var envelope domain.CIREnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return docViewSource{}, domain.ErrIntegrity
	}
	cachedPage := -1
	var entries []domain.DocRangeEntry
	entry := func(position int) (domain.DocRangeEntry, error) {
		if position < 0 || int64(position) >= generation.EventCount {
			return domain.DocRangeEntry{}, domain.ErrValidation
		}
		page := position / domain.DocRangePageEvents
		if page != cachedPage {
			current, err := reader.ReadPage(ctx, page)
			if err != nil {
				return domain.DocRangeEntry{}, err
			}
			if len(current) != min(domain.DocRangePageEvents, int(generation.EventCount)-page*domain.DocRangePageEvents) {
				return domain.DocRangeEntry{}, domain.ErrIntegrity
			}
			for offset, metadata := range current {
				if metadata.Index != page*domain.DocRangePageEvents+offset {
					return domain.DocRangeEntry{}, domain.ErrIntegrity
				}
			}
			cachedPage, entries = page, current
		}
		if len(entries) <= position%domain.DocRangePageEvents {
			return domain.DocRangeEntry{}, domain.ErrIntegrity
		}
		metadata := entries[position%domain.DocRangePageEvents]
		if metadata.Index != position {
			return domain.DocRangeEntry{}, domain.ErrIntegrity
		}
		return metadata, nil
	}
	var chunk func(int) (domain.ConversationManifestChunk, error)
	byteStorage := generation.ByteStorage()
	if byteStorage == domain.DocRangeByteStorageOriginal || byteStorage == domain.DocRangeByteStorageDerived {
		chunk = func(position int) (domain.ConversationManifestChunk, error) { return reader.Chunk(ctx, position) }
	}
	return docViewSource{binding: binding, envelope: envelope, total: int(generation.EventCount), streamBytes: generation.StreamBytes, byteStorage: byteStorage, chunk: chunk, block: func(page int) (domain.ContentHash, error) {
		return reader.PageHash(ctx, page)
	}, entry: entry, location: func(position int) (domain.DocEventIndex, error) {
		metadata, err := entry(position)
		return metadata.DocEventIndex, err
	}}, nil
}
