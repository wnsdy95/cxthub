package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type PreparedMemoryRead struct {
	input         domain.MemoryCaptureBundle
	output        domain.MemoryReadBundle
	qualification domain.MemoryReadQualification
	comparison    *domain.MemoryReadComparison
	graphAnchor   *memoryGraphAnchor
}

func (workers *MemoryPreparationWorkers) PrepareMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource) (*PreparedMemoryRead, error) {
	if _, system := inbound.RepositoryActor(ctx); !system || inbound.DocumentReadAuthorizer(ctx) != nil {
		return nil, domain.ErrForbidden
	}
	var prepared *PreparedMemoryRead
	err := workers.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		captured, err := workers.CaptureEffectiveMemorySource(bound, repo, request, previous)
		if err != nil {
			return err
		}
		prepared, err = prepareCapturedMemoryRead(bound, captured, nil, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (workers *MemoryPreparationWorkers) PublishPreparedMemoryRead(ctx context.Context, prepared *PreparedMemoryRead, expiresAt time.Time) (domain.ContentHash, error) {
	if _, system := inbound.RepositoryActor(ctx); !system || inbound.DocumentReadAuthorizer(ctx) != nil {
		return "", domain.ErrForbidden
	}
	if outbound.DocReadOnly(ctx) {
		return "", domain.ErrConflict
	}
	if prepared == nil || prepared.input.ID != prepared.qualification.CaptureID || prepared.output.ID != prepared.qualification.ReadID {
		return "", domain.ErrIntegrity
	}
	if err := domain.ValidateMemoryReadQualification(ctx, prepared.qualification, prepared.output.Header); err != nil {
		return "", err
	}
	qualified, qualifiedOK := workers.meta.(outbound.MemoryReadQualificationStore)
	if !qualifiedOK {
		return "", domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), workers.DocumentIdentitiesSupported())
	var identifier domain.ContentHash
	err := workers.WithMemoryReadResources(ctx, prepared.qualification.Source.Graph.RepoID, func(bound context.Context) error {
		if err := workers.persistPreparedMemoryRead(bound, prepared, expiresAt); err != nil {
			return err
		}
		var err error
		identifier, err = qualified.QualifyMemoryRead(bound, prepared.qualification)
		return err
	})
	if err != nil {
		return "", err
	}
	return identifier, nil
}

func (workers *MemoryPreparationWorkers) persistPreparedMemoryRead(ctx context.Context, prepared *PreparedMemoryRead, expiresAt time.Time) error {
	if prepared == nil || prepared.input.ID != prepared.qualification.CaptureID || prepared.output.ID != prepared.qualification.ReadID {
		return domain.ErrIntegrity
	}
	if err := domain.ValidateMemoryReadQualification(ctx, prepared.qualification, prepared.output.Header); err != nil {
		return err
	}
	captures, capturesOK := workers.meta.(outbound.MemoryCaptureStore)
	results, resultsOK := workers.meta.(outbound.MemoryReadStore)
	if !capturesOK || !resultsOK {
		return domain.ErrCatalogUnsupported
	}
	if err := captures.WriteMemoryCapture(ctx, prepared.input, expiresAt); err != nil {
		return err
	}
	return results.WriteMemoryRead(ctx, prepared.output)
}

func authorizeMemoryReadSnapshot(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return memoryquery.AuthorizeSnapshot(ctx, repo)
}

func (service *Service) QueryQualifiedMemory(ctx context.Context, repo, qualification domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	queries, err := service.MemoryServingQueries()
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return queries.QueryQualifiedMemory(ctx, repo, qualification, request)
}
