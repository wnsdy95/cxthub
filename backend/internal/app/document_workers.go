package app

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type DocumentWorkersConfig struct {
	Metadata outbound.MetadataStore
	Blobs    outbound.BlobStore
	Verifier *domain.CanonicalDocVerifier
	Profile  documentquery.Profile
}

// DocumentWorkers holds the worker that turns the chunks of an upload into a
// stored conversation document.
type DocumentWorkers struct {
	Finalization *documentworker.Finalizer
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
	scope := newDocumentWorkerScope(config.Metadata, config.Blobs, config.Profile)
	for _, optional := range []any{jobs, scope.documents.transactions, scope.documents.presence} {
		if optional != nil && !rootAdapterPresent(optional) {
			return nil, domain.ErrValidation
		}
	}
	if scope.documents.transactions != nil && scope.documents.presence == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Profile == documentquery.VerifiedRoots {
		for _, required := range []any{jobs, scope.documents.transactions, scope.documents.presence} {
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
	return &DocumentWorkers{Finalization: finalization}, nil
}

func (service *Service) DocumentWorkersForProfile(profile documentquery.Profile) (*DocumentWorkers, error) {
	return NewDocumentWorkers(DocumentWorkersConfig{
		Metadata: service.meta, Blobs: service.blobs, Verifier: &service.docVerifier, Profile: profile,
	})
}
