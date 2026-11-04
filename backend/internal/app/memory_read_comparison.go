package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (workers *MemoryPreparationWorkers) memoryReadReference(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.ContentHash, error) {
	transactions, supported := workers.meta.(outbound.RepositoryTransactionState)
	if !supported || !transactions.InReadOnlyTransaction(ctx) {
		return "", domain.ErrConflict
	}
	request, err := normalizeEffectiveMemoryRequest(repo, request)
	if err != nil {
		return "", err
	}
	if request.Cursor != "" {
		return "", domain.ErrValidation
	}
	request.Limit = 1
	queries, err := workers.memoryLiveQueries(true, false)
	if err != nil {
		return "", err
	}
	page, err := queries.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		return "", err
	}
	if page.DeliveryStateHash == "" {
		return "", domain.ErrMemoryProjectionLimit
	}
	repository, err := workers.graphEvidenceReader().GetRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	return domain.MemoryReadComparisonStateHash(ctx, repo, repository.GitRemoteURL, page)
}

func newMemoryReadComparison(ctx context.Context, output domain.MemoryReadBundle, expected domain.ContentHash) (*domain.MemoryReadComparison, error) {
	header := output.Header
	target, err := domain.MemoryReadTargetID(ctx, domain.MemoryReadTarget{Version: domain.MemoryReadWorkVersion, Rules: header.Rules,
		RepoID: header.Source.Graph.RepoID, Selection: header.View.Selection, Content: header.View.Content})
	if err != nil {
		return nil, err
	}
	actual, err := domain.MemoryReadComparisonStateHash(ctx, header.Source.Graph.RepoID, header.Origin, header.View)
	if err != nil {
		return nil, err
	}
	comparison := domain.MemoryReadComparison{Version: domain.MemoryReadComparisonVersion, ReadID: output.ID,
		CaptureID: header.CaptureID, TargetID: target, Source: header.Source, Expected: expected, Actual: actual}
	if err := domain.ValidateMemoryReadComparison(ctx, comparison, header); err != nil {
		return nil, err
	}
	return &comparison, nil
}
