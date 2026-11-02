package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docViewBytes = 128 << 10
const docViewScanEvents = 512

var errDocViewGenerationNeedsPreparation = fmt.Errorf("%w: document generation is not ready", domain.ErrDocRangeIndexUnavailable)

type docRangeRequirement uint8

const (
	docRangeRootOnly docRangeRequirement = iota
	docRangeLegacyMetadata
	docRangeLegacyBytes
)

func (service *Service) docBytePreparation() (outbound.DocByteRangePreparationStore, bool) {
	storage, ok := service.blobs.(outbound.DocByteRangePreparationStore)
	return storage, ok && storage.LegacyByteRangePublicationEnabled()
}

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

type docViewRange struct {
	binding     domain.DocReadGenerationBinding
	envelope    json.RawMessage
	total       int
	streamBytes int64
	readPage    func(int) ([]domain.DocRangeEntry, error)
	block       func(int) (domain.ContentHash, error)
	chunk       func(int) (domain.ConversationManifestChunk, error)
}

func openDocViewRange(ctx context.Context, storage outbound.DocRangeStore, repo, hash domain.ContentHash) (docViewRange, error) {
	if generations, ok := storage.(outbound.DocRangeGenerationStore); ok {
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
	if snapshots, ok := storage.(outbound.DocRangeSnapshotStore); ok && snapshots.InReadOnlyTransaction(ctx) {
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

func (service *Service) prepareDocViewSource(ctx context.Context, repo, hash domain.ContentHash, requirement docRangeRequirement) (bool, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return false, err
	}
	var prepared, needs bool
	bytes, byteEnabled := service.docBytePreparation()
	requireBytes := false
	err := resourceWork(ctx, ResourceInteractive, func(admitted context.Context) error {
		var err error
		admitted, err = reauthorizeDocumentRead(admitted, repo)
		if err != nil {
			return err
		}
		needs, err = repositoryReadForRepo(admitted, service, repo, func(read context.Context) (bool, error) {
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
				if requirement == docRangeRootOnly {
					return false, nil
				}
				generations, available := service.blobs.(outbound.DocRangeGenerationStore)
				_, queued := service.blobs.(outbound.DocRangeJobStore)
				if !available || !queued {
					return false, nil
				}
				if !generations.InReadOnlyTransaction(read) {
					return false, domain.ErrConflict
				}
				readiness, available := service.blobs.(outbound.DocRangeReadinessStore)
				if !available {
					return false, domain.ErrDocRangeIndexUnavailable
				}
				prepared = true
				requireBytes = requirement == docRangeLegacyBytes && byteEnabled
				if requireBytes {
					ready, err := bytes.HasDocByteRangeIndex(read, repo, hash)
					return !ready, err
				}
				ready, err := readiness.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			prepared = true
			storage, ok := service.blobs.(outbound.DocRangeStore)
			if !ok {
				return false, domain.ErrDocRangeIndexUnavailable
			}
			if readiness, ok := storage.(outbound.DocRangeReadinessStore); ok {
				ready, err := readiness.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			_, err = storage.DocRangeIndex(read, repo, hash)
			if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				return true, nil
			}
			return false, err
		})
		return err
	})
	if err != nil || !needs {
		return prepared, err
	}
	if outbound.DocReadOnly(ctx) {
		return prepared, domain.ErrDocRangeIndexUnavailable
	}
	if jobs, ok := service.blobs.(outbound.DocRangeJobStore); ok {
		return prepared, resourceWork(ctx, ResourceInteractive, func(admitted context.Context) error {
			admitted, err := reauthorizeDocumentRead(admitted, repo)
			if err != nil {
				return err
			}
			var job domain.DocRangeIndexJob
			if requireBytes {
				job, err = bytes.EnqueueDocByteRangeJob(admitted, repo, hash, time.Now().UTC())
			} else {
				job, err = jobs.EnqueueDocRangeJob(admitted, repo, hash, time.Now().UTC())
			}
			if err != nil {
				return err
			}
			return docRangePreparationError(job)
		})
	}
	return prepared, resourceWork(ctx, ResourceBulk, func(admitted context.Context) error {
		return service.blobs.(outbound.DocRangeStore).EnsureDocRangeIndex(admitted, repo, hash)
	})
}

func reauthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	return ctx, ctx.Err()
}

func (service *Service) docViewSource(ctx context.Context, repo, hash domain.ContentHash, legacyCompatibility bool) (docViewSource, error) {
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return docViewSource{}, err
	}
	if err != nil {
		return service.docViewSnapshotSource(ctx, repo, hash, nil, legacyCompatibility)
	}
	return service.docViewSnapshotSource(ctx, repo, hash, &snapshot, legacyCompatibility)
}

func (service *Service) docViewSnapshotSource(ctx context.Context, repo, hash domain.ContentHash, snapshot *domain.Snapshot, legacyCompatibility bool) (docViewSource, error) {
	if snapshot != nil {
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, *snapshot); err != nil {
			return docViewSource{}, err
		}
	}
	if snapshot != nil && snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
		storage, ok := service.blobs.(outbound.DocRangeStore)
		if !ok {
			return docViewSource{}, domain.ErrDocRangeIndexUnavailable
		}
		chunks, ok := service.blobs.(outbound.ConversationChunkReader)
		if !ok {
			return docViewSource{}, domain.ErrUnsupportedDocumentIdentity
		}
		index, err := openDocViewRange(ctx, storage, repo, hash)
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
	if storage, ok := service.blobs.(outbound.DocRangeGenerationStore); ok && storage.InReadOnlyTransaction(ctx) {
		if source, ready, err := service.legacySnapshotGenerationSource(ctx, repo, hash, snapshot); ready || err != nil {
			if _, enabled := service.docBytePreparation(); err == nil && ready && enabled && !legacyCompatibility && source.chunk == nil && source.streamBytes > 0 {
				return docViewSource{}, errDocViewGenerationNeedsPreparation
			}
			return source, err
		}
		if _, queued := service.blobs.(outbound.DocRangeJobStore); queued && snapshot != nil && !legacyCompatibility {
			return docViewSource{}, errDocViewGenerationNeedsPreparation
		}
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
	return legacyDocViewSource(source), nil
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

func docViewInherited(ctx context.Context, source, base docViewSource) (int, error) {
	inherited, total := 0, min(source.total, base.total)
	for inherited < total {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		page := inherited / domain.DocRangePageEvents
		if inherited%domain.DocRangePageEvents == 0 && source.block != nil && base.block != nil {
			left, err := source.block(page)
			if err != nil {
				return 0, err
			}
			right, err := base.block(page)
			if err != nil {
				return 0, err
			}
			if left == right {
				inherited = min(total, inherited+domain.DocRangePageEvents)
				continue
			}
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
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewPage{}, err
	}
	return queries.ReadDocViewEvents(ctx, repo, hash, base, offset, limit, view)
}

func (service *Service) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	return queries.ReadDocViewFragment(ctx, repo, hash, index, offset, budget)
}
