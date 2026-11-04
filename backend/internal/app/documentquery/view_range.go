package documentquery

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewRange struct {
	binding     domain.DocReadGenerationBinding
	envelope    json.RawMessage
	total       int
	streamBytes int64
	readPage    func(int) ([]domain.DocRangeEntry, error)
	block       func(int) (domain.ContentHash, error)
	chunk       func(int) (domain.ConversationManifestChunk, error)
}

func openDocViewRange(ctx context.Context, storage DocRangeReader, snapshots outbound.DocRangeSnapshotStore, generations outbound.DocRangeGenerationStore, repo, hash domain.ContentHash) (docViewRange, error) {
	if generations != nil {
		if !generations.InReadOnlyTransaction(ctx) {
			return docViewRange{}, domain.ErrConflict
		}
		read, err := generations.OpenDocRangeGeneration(ctx, repo, hash)
		if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			return docViewRange{}, errDocViewGenerationNeedsPreparation
		}
		if err != nil {
			return docViewRange{}, err
		}
		descriptor := read.Descriptor()
		if err := descriptor.Validate(hash); err != nil {
			return docViewRange{}, err
		}
		if descriptor.Identity() != domain.DocumentIdentityRootV1 {
			return docViewRange{}, domain.ErrIntegrity
		}
		envelope, err := read.Envelope(ctx)
		if err != nil {
			return docViewRange{}, err
		}
		return docViewRange{binding: read.Binding(), envelope: envelope, total: int(descriptor.EventCount), streamBytes: descriptor.StreamBytes,
			readPage: func(page int) ([]domain.DocRangeEntry, error) { return read.ReadPage(ctx, page) },
			block:    func(page int) (domain.ContentHash, error) { return read.PageHash(ctx, page) },
			chunk:    func(position int) (domain.ConversationManifestChunk, error) { return read.Chunk(ctx, position) }}, nil
	}
	var index domain.DocRangeIndex
	var readPage func(int) ([]domain.DocRangeEntry, error)
	if snapshots != nil && snapshots.InReadOnlyTransaction(ctx) {
		read, err := snapshots.OpenDocRange(ctx, repo, hash)
		if err != nil {
			return docViewRange{}, err
		}
		index = read.Header()
		readPage = func(page int) ([]domain.DocRangeEntry, error) {
			return read.ReadPage(ctx, page)
		}
	} else {
		var err error
		index, err = storage.DocRangeIndex(ctx, repo, hash)
		if err != nil {
			return docViewRange{}, err
		}
		readPage = func(page int) ([]domain.DocRangeEntry, error) {
			return storage.DocRangePage(ctx, repo, index, page)
		}
	}
	if err := index.Validate(hash); err != nil {
		return docViewRange{}, err
	}
	return docViewRange{envelope: index.Manifest.Envelope, total: int(index.Manifest.EventCount), streamBytes: index.Manifest.StreamBytes, readPage: readPage,
		block: func(page int) (domain.ContentHash, error) {
			if page < 0 || page >= len(index.Pages) {
				return "", domain.ErrIntegrity
			}
			return index.Pages[page], nil
		}, chunk: func(position int) (domain.ConversationManifestChunk, error) {
			if position < 0 || position >= len(index.Manifest.Chunks) {
				return domain.ConversationManifestChunk{}, domain.ErrIntegrity
			}
			return index.Manifest.Chunks[position], nil
		}}, nil
}

func docChunkRangeReader(ctx context.Context, streamBytes int64, locate func(int) (domain.ConversationManifestChunk, error), load func(context.Context, int, domain.ConversationManifestChunk) ([]byte, error)) func(int, int) ([]byte, error) {
	cachedChunk := -1
	var body []byte
	return func(offset, length int) ([]byte, error) {
		if offset < 0 || length <= 0 || length > docViewBytes || int64(offset) > streamBytes-int64(length) {
			return nil, domain.ErrValidation
		}
		result := make([]byte, 0, length)
		for position := offset; position < offset+length; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			chunk := position / domain.ConversationManifestChunkBytes
			if cachedChunk != chunk {
				descriptor, err := locate(chunk)
				if err != nil {
					return nil, err
				}
				if descriptor.Bytes != min(int64(domain.ConversationManifestChunkBytes), streamBytes-int64(chunk)*domain.ConversationManifestChunkBytes) {
					return nil, domain.ErrIntegrity
				}
				body, err = load(ctx, chunk, descriptor)
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
