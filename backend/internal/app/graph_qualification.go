package app

import (
	"context"
	"log"
	"slices"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigureGraphQualifications(ctx context.Context, repos []domain.ContentHash) error {
	if len(repos) == 0 {
		service.graphQualificationRepos = nil
		return nil
	}
	storage, ok := service.meta.(outbound.GraphQualificationStore)
	if !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.GraphProjectionStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.GraphProjectionRepairStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.RepositoryGraphSourceGenerationStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.RepositoryTransactions); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.RepositoryTransactionPresence); !ok {
		return domain.ErrCatalogUnsupported
	}
	configured := slices.Clone(repos)
	slices.Sort(configured)
	configured = slices.Compact(configured)
	for _, repo := range configured {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
	}
	ctx = service.DocumentIdentityWorkerContext(inbound.WithSystemActor(ctx))
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), service.DocumentIdentitiesSupported())
	for _, repo := range configured {
		if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
			return err
		}
	}
	service.graphQualificationRepos = configured
	service.graphQualificationNext.Store(0)
	return nil
}

func (service *Service) ProcessGraphQualifications(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	if len(service.graphQualificationRepos) == 0 {
		return nil
	}
	worker, err := projectionQualification(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		return err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) qualificationWorker(storage outbound.GraphQualificationStore) (*projectionworker.Qualification, error) {
	scope := newProjectionWorkerScope(service.meta, service.blobs)
	projections, _ := service.meta.(outbound.GraphProjectionStore)
	repairs, _ := service.meta.(outbound.GraphProjectionRepairStore)
	generations, _ := service.meta.(outbound.RepositoryGraphSourceGenerationStore)
	return projectionworker.NewQualification(projectionworker.QualificationConfig{
		Profile: projectionworker.Compatibility, Scope: scope, Jobs: storage, Projections: projections, Repairs: repairs,
		Generations: generations, Reference: newProjectionWorkerReference(service.meta, scope),
		Repositories: service.graphQualificationRepos, Next: &service.graphQualificationNext,
	})
}

func (service *Service) runGraphQualification(ctx context.Context, storage outbound.GraphQualificationStore, lease domain.GraphQualificationLease) error {
	worker, err := service.qualificationWorker(storage)
	if err != nil {
		return err
	}
	return worker.RunLease(ctx, lease)
}

func (service *Service) repairGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, readErr error) error {
	storage, _ := service.meta.(outbound.GraphQualificationStore)
	worker, err := service.qualificationWorker(storage)
	if err != nil {
		return err
	}
	return worker.Repair(ctx, lease, readErr)
}

func graphQualificationCorruption(lease domain.GraphQualificationLease, readErr error) *domain.GraphProjectionCorruption {
	return projectionworker.QualificationCorruption(lease, readErr)
}

func (service *Service) compareGraphQualification(ctx context.Context, lease domain.GraphQualificationLease) (domain.GraphQualificationResult, error) {
	storage, _ := service.meta.(outbound.GraphQualificationStore)
	worker, err := service.qualificationWorker(storage)
	if err != nil {
		return domain.GraphQualificationResult{}, err
	}
	return worker.Compare(ctx, lease)
}

func (service *Service) checkGraphQualification(ctx context.Context, projection domain.GraphProjectionRead) error {
	return domain.ValidateGraphReadQualification(ctx, projection)
}

func (service *Service) RunGraphQualificationWorker(ctx context.Context) {
	if len(service.graphQualificationRepos) == 0 {
		return
	}
	worker, err := projectionQualification(service.projectionWorkersConfig(projectionworker.Compatibility), newProjectionWorkerScope(service.meta, service.blobs))
	if err != nil {
		log.Printf("graph qualification: %v", err)
		return
	}
	worker.Run(ctx)
}
