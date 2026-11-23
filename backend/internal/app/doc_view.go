package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

func reauthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	return ctx, ctx.Err()
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
