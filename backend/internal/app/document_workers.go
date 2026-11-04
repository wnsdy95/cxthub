package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type DocumentWorkersConfig struct {
	Metadata   outbound.MetadataStore
	Blobs      outbound.BlobStore
	FindFields outbound.DocFindFieldsStore
	Display    outbound.DocDisplayStore
	Verifier   *domain.CanonicalDocVerifier
	Profile    documentquery.Profile
}

type DocumentWorkers struct {
	Finalization *documentworker.Finalizer
	Range        *documentworker.RangeWorker
	FindFields   *documentworker.FindFieldsWorker
	Display      *documentworker.DisplayWorker
}

type legacyDocumentWorkerSources struct {
	metadata outbound.MetadataStore
	blobs    outbound.BlobStore
	scope    *documentQueryScope
}

var _ documentworker.SourceReader = (*legacyDocumentWorkerSources)(nil)

func (sources *legacyDocumentWorkerSources) queries(ctx context.Context) (*documentquery.Queries, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !rootAdapterPresent(sources.metadata) || !rootAdapterPresent(sources.blobs) {
		return nil, domain.ErrCatalogUnsupported
	}
	return newReadServicesDocuments(ReadServicesConfig{
		Metadata: sources.metadata, Blobs: sources.blobs, DocumentProfile: documentquery.LegacyCompatibility,
	}, sources.scope)
}

func (sources *legacyDocumentWorkerSources) OpenSource(ctx context.Context, repo, hash domain.ContentHash) (documentquery.Source, error) {
	queries, err := sources.queries(ctx)
	if err != nil {
		return documentquery.Source{}, err
	}
	return queries.OpenSource(ctx, repo, hash)
}

func (sources *legacyDocumentWorkerSources) OpenViewSource(ctx context.Context, repo, hash domain.ContentHash) (documentquery.Source, error) {
	queries, err := sources.queries(ctx)
	if err != nil {
		return documentquery.Source{}, err
	}
	return queries.OpenViewSource(ctx, repo, hash)
}

func NewDocumentWorkers(config DocumentWorkersConfig) (*DocumentWorkers, error) {
	if config.Profile != documentquery.LegacyCompatibility && config.Profile != documentquery.VerifiedRoots {
		return nil, domain.ErrValidation
	}
	for _, required := range []any{config.Metadata, config.Blobs, config.Verifier} {
		if !rootAdapterPresent(required) {
			return nil, fmt.Errorf("%w: document worker dependencies are required", domain.ErrValidation)
		}
	}
	jobs, _ := config.Blobs.(outbound.DocJobStore)
	ranges, _ := config.Blobs.(outbound.DocRangeJobStore)
	scope := newDocumentWorkerScope(config.Metadata, config.Blobs, config.Profile)
	for _, optional := range []any{jobs, ranges, config.FindFields, config.Display, scope.documents.transactions, scope.documents.presence} {
		if optional != nil && !rootAdapterPresent(optional) {
			return nil, domain.ErrValidation
		}
	}
	if scope.documents.transactions != nil && scope.documents.presence == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Profile == documentquery.VerifiedRoots {
		for _, required := range []any{jobs, ranges, config.FindFields, config.Display, scope.documents.transactions, scope.documents.presence} {
			if !rootAdapterPresent(required) {
				return nil, domain.ErrCatalogUnsupported
			}
		}
		if !hasDocumentIdentity(scope.documents.supported, domain.DocumentIdentityRootV1) {
			return nil, domain.ErrUnsupportedDocumentIdentity
		}
	}
	finalization, err := documentworker.NewFinalizer(jobs, config.Blobs, config.Verifier, scope)
	if err != nil {
		return nil, err
	}
	sources, err := newDocumentWorkerSources(config, scope)
	if err != nil {
		return nil, err
	}
	workers := &DocumentWorkers{Finalization: finalization}
	if ranges != nil {
		workers.Range = documentworker.NewRangeWorker(ranges, scope)
	}
	if config.FindFields != nil {
		workers.FindFields = documentworker.NewFindFieldsWorker(config.FindFields, sources, scope)
	}
	if config.Display != nil {
		workers.Display = documentworker.NewDisplayWorker(config.Display, sources, scope)
	}
	return workers, nil
}

func newDocumentWorkerSources(config DocumentWorkersConfig, scope *documentWorkerScope) (*documentquery.Queries, error) {
	return newReadServicesDocuments(ReadServicesConfig{
		Metadata: config.Metadata, Blobs: config.Blobs, DocumentProfile: config.Profile,
		DocumentFindFields: config.FindFields, DocumentDisplayReads: config.Display,
	}, scope.documents)
}

func (service *Service) DocumentWorkersForProfile(profile documentquery.Profile) (*DocumentWorkers, error) {
	return NewDocumentWorkers(DocumentWorkersConfig{
		Metadata: service.meta, Blobs: service.blobs, FindFields: service.docFindFields,
		Display: service.docDisplay, Verifier: &service.docVerifier, Profile: profile,
	})
}

func (service *Service) documentFinalizer(storage outbound.DocJobStore) (*documentworker.Finalizer, error) {
	scope := newDocumentWorkerScope(service.meta, service.blobs, documentquery.LegacyCompatibility)
	return documentworker.NewFinalizer(storage, service.blobs, &service.docVerifier, scope)
}

func (service *Service) documentRangeWorker(storage outbound.DocRangeJobStore) *documentworker.RangeWorker {
	return documentworker.NewRangeWorker(storage, newDocumentWorkerScope(service.meta, service.blobs, documentquery.LegacyCompatibility))
}

func (service *Service) documentFindFieldsWorker() (*documentworker.FindFieldsWorker, error) {
	scope := newDocumentWorkerScope(service.meta, service.blobs, documentquery.LegacyCompatibility)
	sources, err := service.documentWorkerSources(scope)
	if err != nil {
		return nil, err
	}
	return documentworker.NewFindFieldsWorker(service.docFindFields, sources, scope), nil
}

func (service *Service) documentDisplayWorker() (*documentworker.DisplayWorker, error) {
	scope := newDocumentWorkerScope(service.meta, service.blobs, documentquery.LegacyCompatibility)
	sources, err := service.documentWorkerSources(scope)
	if err != nil {
		return nil, err
	}
	return documentworker.NewDisplayWorker(service.docDisplay, sources, scope), nil
}

func (service *Service) documentWorkerSources(scope *documentWorkerScope) (documentworker.SourceReader, error) {
	return &legacyDocumentWorkerSources{metadata: service.meta, blobs: service.blobs, scope: scope.documents}, nil
}
