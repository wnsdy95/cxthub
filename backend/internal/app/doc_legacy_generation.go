package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) legacyGenerationSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, bool, error) {
	if _, supported := service.blobs.(outbound.DocRangeGenerationStore); !supported {
		return docViewSource{}, false, nil
	}
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docViewSource{}, false, err
	}
	if err != nil {
		return service.legacySnapshotGenerationSource(ctx, repo, hash, nil)
	}
	return service.legacySnapshotGenerationSource(ctx, repo, hash, &snapshot)
}

func (service *Service) legacySnapshotGenerationSource(ctx context.Context, repo, hash domain.ContentHash, snapshot *domain.Snapshot) (docViewSource, bool, error) {
	storage, ok := service.blobs.(outbound.DocRangeGenerationStore)
	if !ok {
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
		bound, release, err := AcquireResources(ctx, ResourceBulk)
		if err != nil {
			return nil, err
		}
		defer release()
		if read == nil {
			switch source.byteStorage {
			case domain.DocRangeByteStorageOriginal:
				chunks, available := service.blobs.(outbound.ConversationChunkReader)
				if !available {
					return nil, domain.ErrUnsupportedDocumentIdentity
				}
				read = docChunkRangeReader(outbound.WithDocReadOnly(bound), source.streamBytes, source.chunk, func(current context.Context, _ int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
					return chunks.ReadConversationChunk(current, repo, descriptor.Hash, descriptor.Bytes)
				})
			case domain.DocRangeByteStorageDerived:
				chunks, available := service.blobs.(outbound.LegacyDerivedByteRangeReader)
				if !available {
					return nil, domain.ErrUnsupportedDocumentIdentity
				}
				read = docChunkRangeReader(outbound.WithDocReadOnly(bound), source.streamBytes, source.chunk, func(current context.Context, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
					return chunks.ReadLegacyDerivedByteChunk(current, repo, hash, source.binding, ordinal, descriptor)
				})
			case domain.DocRangeByteStorageNone:
				read, err = service.eventRangeReader(outbound.WithDocReadOnly(bound), repo, hash)
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
