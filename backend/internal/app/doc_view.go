package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
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

type docViewSource struct {
	envelope    domain.CIREnvelope
	total       int
	root        bool
	streamBytes int64
	chunk       func(int) (domain.ConversationManifestChunk, error)
	block       func(int) (domain.ContentHash, error)
	entry       func(int) (domain.DocRangeEntry, error)
	location    func(int) (domain.DocEventIndex, error)
	read        func(int, int) ([]byte, error)
}

type docViewRange struct {
	envelope    json.RawMessage
	total       int
	streamBytes int64
	readPage    func(int) ([]domain.DocRangeEntry, error)
	block       func(int) (domain.ContentHash, error)
	chunk       func(int) (domain.ConversationManifestChunk, error)
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

func (service *Service) ReadDocViewEvents(ctx context.Context, repo, hash domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewPage{}, err
	}
	return queries.ReadDocViewEvents(ctx, repo, hash, offset, limit, view)
}

func (service *Service) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	return queries.ReadDocViewFragment(ctx, repo, hash, index, offset, budget)
}
