package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var (
	_ inbound.GitChangeQuery         = (*evidencequery.ChangeQueries)(nil)
	_ inbound.GitScanQuery           = (*evidencequery.ScanQueries)(nil)
	_ inbound.CodeApplicabilityQuery = (*evidencequery.CodeQueries)(nil)
)

type statusQueryScope struct {
	repository *repositoryQueryScope
}

func (scope *statusQueryScope) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.repository.WithinRepositoryRead(ctx, repo, read)
}

func (service *Service) statusQueryScope() *statusQueryScope {
	transactions, _ := service.meta.(documentQuerySnapshots)
	return &statusQueryScope{repository: &repositoryQueryScope{
		metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported(),
	}}
}

func newGitScanQueries(service *Service, scans evidencequery.ScanReader) (*evidencequery.ScanQueries, error) {
	scope := service.statusQueryScope()
	heads, _ := service.meta.(evidencequery.HeadScanReader)
	return evidencequery.NewScanQueries(evidencequery.ScanConfig{
		Scope: scope, Metadata: service.meta, Scans: scans, Heads: heads, AtomicRead: scope.repository.transactions != nil,
	})
}

func newGitChangeQueries(service *Service, changes evidencequery.ChangeReader) (*evidencequery.ChangeQueries, error) {
	scope := service.statusQueryScope()
	return evidencequery.NewChangeQueries(evidencequery.ChangeConfig{
		Scope: scope, Changes: changes, AtomicRead: scope.repository.transactions != nil,
	})
}

func (service *Service) CodeQueries() (*evidencequery.CodeQueries, error) {
	scope := service.statusQueryScope()
	reader := service.graphEvidenceReader()
	trees, _ := reader.(evidencequery.GitTreeReader)
	deltas, _ := reader.(evidencequery.GitDeltaReader)
	revisions, _ := service.meta.(evidencequery.RevisionReader)
	return evidencequery.NewCodeQueries(evidencequery.CodeConfig{
		Scope: scope, Metadata: reader, Revisions: revisions, Trees: trees, Deltas: deltas,
		RequireComplete: service.requireCompleteMemory, AtomicRead: scope.repository.transactions != nil,
	})
}
