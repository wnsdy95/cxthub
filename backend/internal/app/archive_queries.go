package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func NewArchiveQueries(config archivequery.Config) (inbound.RepositoryArchiveQueries, error) {
	queries, err := archivequery.New(config)
	if err != nil {
		return nil, err
	}
	return queries, nil
}

func (service *Service) ArchiveQueries() (inbound.RepositoryArchiveQueries, error) {
	if service == nil || !rootAdapterPresent(service.meta) || !rootAdapterPresent(service.blobs) {
		return nil, domain.ErrCatalogUnsupported
	}
	transactions, _ := service.meta.(documentQuerySnapshots)
	presence, _ := service.meta.(outbound.RepositoryTransactionPresence)
	repository := &repositoryQueryScope{metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported()}
	document := &documentQueryScope{metadata: service.meta, transactions: transactions, presence: presence, supported: repository.supported}
	indexed, _ := service.blobs.(outbound.DocReadStore)
	verified, _ := service.blobs.(outbound.VerifiedDocReader)
	raw, err := documentquery.NewRawReader(documentquery.RawConfig{
		Profile: documentquery.LegacyCompatibility, Snapshots: service.meta, Legacy: service.blobs, Index: indexed, Verified: verified,
	})
	if err != nil {
		return nil, err
	}
	candidates, _ := service.blobs.(archivequery.CandidateReader)
	return NewArchiveQueries(archivequery.Config{
		Profile:  documentquery.LegacyCompatibility,
		Scope:    &archiveQueryScope{repository: repository, document: document, presence: presence},
		Metadata: service.meta, Documents: raw, Candidates: candidates, AtomicRead: transactions != nil,
	})
}
