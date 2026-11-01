package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type projectionWorkerScope struct {
	metadata     documentQueryRepoReader
	transactions outbound.RepositoryTransactions
	presence     outbound.RepositoryTransactionPresence
	state        outbound.RepositoryTransactionState
	supported    []domain.DocumentIdentity
}

var _ projectionworker.GraphScope = (*projectionWorkerScope)(nil)
var _ projectionworker.QualificationScope = (*projectionWorkerScope)(nil)

func newProjectionWorkerScope(metadata outbound.MetadataStore, blobs outbound.BlobStore) *projectionWorkerScope {
	transactions, _ := metadata.(outbound.RepositoryTransactions)
	presence, _ := metadata.(outbound.RepositoryTransactionPresence)
	state, _ := metadata.(outbound.RepositoryTransactionState)
	return &projectionWorkerScope{metadata: metadata, transactions: transactions, presence: presence, state: state,
		supported: append([]domain.DocumentIdentity(nil), ReadDocumentIdentities(metadata, blobs)...)}
}

func (scope *projectionWorkerScope) WorkerContext(ctx context.Context) context.Context {
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), scope.supported)
	return outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.supported)
}

func (*projectionWorkerScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	return admittedWork(ctx, work)
}

func (*projectionWorkerScope) AcquireBuild(ctx context.Context) (context.Context, func(), error) {
	return acquireGraphBuild(ctx)
}

func (scope *projectionWorkerScope) WithinRead(ctx context.Context, read func(context.Context) error) error {
	execute := func(bound context.Context) error {
		current, cancel := context.WithCancel(bound)
		defer cancel()
		return read(current)
	}
	if scope.transactions != nil {
		return scope.transactions.WithinReadSnapshot(ctx, execute)
	}
	return execute(ctx)
}

func (scope *projectionWorkerScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.supported)
	return scope.WithinRead(ctx, func(bound context.Context) error {
		if err := checkRepositoryDocumentIdentity(bound, scope.metadata, scope.supported, repo, true); err != nil {
			return err
		}
		return read(bound)
	})
}

func (scope *projectionWorkerScope) CheckRepository(ctx context.Context, repo domain.ContentHash) error {
	return scope.WithinRepositoryRead(ctx, repo, func(context.Context) error { return nil })
}

func (scope *projectionWorkerScope) CheckIdentity(ctx context.Context, repo domain.ContentHash) error {
	return checkRepositoryDocumentIdentity(ctx, scope.metadata, scope.supported, repo, false)
}

func (scope *projectionWorkerScope) ValidateQualificationStart(ctx context.Context) error {
	_, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	if scope.presence == nil || scope.presence.InTransaction(ctx) || writing || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	return nil
}

func (scope *projectionWorkerScope) InReadOnlyTransaction(ctx context.Context) bool {
	return scope.state != nil && scope.state.InReadOnlyTransaction(ctx)
}

type projectionWorkerReference struct {
	metadata outbound.MetadataStore
	scope    *projectionWorkerScope
	budget   outbound.GraphQualificationStore
	history  repositoryquery.HistoryReader
}

func newProjectionWorkerReference(metadata outbound.MetadataStore, scope *projectionWorkerScope) *projectionWorkerReference {
	budget, _ := metadata.(outbound.GraphQualificationStore)
	history, _ := metadata.(repositoryquery.HistoryReader)
	return &projectionWorkerReference{metadata: metadata, scope: scope, budget: budget, history: history}
}

func (reference *projectionWorkerReference) Reference(ctx context.Context, repo domain.ContentHash) (domain.ContentHash, error) {
	if !reference.scope.InReadOnlyTransaction(ctx) {
		return "", domain.ErrConflict
	}
	if !rootAdapterPresent(reference.budget) {
		return "", domain.ErrCatalogUnsupported
	}
	if err := reference.budget.CheckGraphQualificationInputs(ctx, repo); err != nil {
		return "", err
	}
	if err := reference.scope.CheckIdentity(ctx, repo); err != nil {
		return "", err
	}
	revisions, _ := reference.metadata.(repositoryquery.RevisionReader)
	archives, _ := reference.metadata.(repositoryquery.ArchiveReader)
	view, err := repositoryquery.LoadRepositoryView(ctx, repo, repositoryquery.RepositoryViewReaders{
		Metadata: reference.metadata, History: reference.history, Revisions: revisions, Archives: archives,
	})
	if err != nil {
		return "", err
	}
	contributions, err := newReadServicesContributions(reference.metadata)
	if err != nil {
		return "", err
	}
	scope := &legacyGraphProjectionScope{state: reference.scope.state}
	generations, _ := reference.metadata.(repositoryquery.GraphGenerationReader)
	trees, _ := reference.metadata.(evidencequery.GitTreeReader)
	deltas, _ := reference.metadata.(evidencequery.GitDeltaReader)
	projection, err := repositoryquery.NewLegacyProjection(repositoryquery.LegacyProjectionConfig{
		Scope: scope, Resources: scope, Metadata: reference.metadata, Contributions: contributions,
		Generations: generations, Trees: trees, Deltas: deltas,
		Graphs: &repositoryquery.GraphCache{}, Branches: &repositoryquery.BranchCache{},
	})
	if err != nil {
		return "", err
	}
	graph, err := projection.Project(ctx, view, "")
	if err != nil {
		return "", err
	}
	view.Graph = &graph
	return domain.GraphViewDigest(ctx, repo, view)
}
