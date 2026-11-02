package memoryquery

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type LiveSourceMode uint8

const (
	LiveCurrent LiveSourceMode = iota + 1
	LiveFrozen
)

type LiveHistoryReader interface {
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}

type LiveRevisionReader interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}

type MemoryProofScope interface {
	CacheableMemoryProof(context.Context, domain.ContentHash) bool
}

type LiveGenerationScope interface {
	IsPinnedRead(context.Context) bool
}

type LiveGraphGenerationReader interface {
	ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error)
}

type LiveConfig struct {
	Mode              LiveSourceMode
	Scope             SnapshotScope
	Metadata          outbound.MemoryMetadataReader
	Projection        outbound.MemoryProjectionReader
	History           LiveHistoryReader
	Revisions         LiveRevisionReader
	Repository        evidencequery.MetadataReader
	Evidence          *evidencequery.Queries
	Trees             evidencequery.GitTreeReader
	Deltas            evidencequery.GitDeltaReader
	GraphScope        LiveGenerationScope
	GraphGenerations  LiveGraphGenerationReader
	MemoryGenerations outbound.RepositoryMemorySourceGenerationStore
	PRPins            outbound.PRContributionMemoryReader
	GitPins           outbound.GitContributionMemoryReader
	Delivery          *DeliveryCache
	ProofScope        MemoryProofScope
	FrozenGeneration  *domain.RepositoryMemorySourceGeneration
	CapturedEvidence  bool
	RequireComplete   bool
}

type LiveQueries struct {
	mode              LiveSourceMode
	scope             SnapshotScope
	metadata          outbound.MemoryMetadataReader
	projection        outbound.MemoryProjectionReader
	history           LiveHistoryReader
	revisions         LiveRevisionReader
	repository        evidencequery.MetadataReader
	evidence          *evidencequery.Queries
	trees             evidencequery.GitTreeReader
	deltas            evidencequery.GitDeltaReader
	graphScope        LiveGenerationScope
	graphGenerations  LiveGraphGenerationReader
	memoryGenerations outbound.RepositoryMemorySourceGenerationStore
	prPins            outbound.PRContributionMemoryReader
	gitPins           outbound.GitContributionMemoryReader
	delivery          *DeliveryCache
	proofScope        MemoryProofScope
	frozenGeneration  *domain.RepositoryMemorySourceGeneration
	capturedEvidence  bool
	requireComplete   bool
}

var _ inbound.EffectiveMemoryQuery = (*LiveQueries)(nil)
var _ inbound.BranchMemoryQuery = (*LiveQueries)(nil)
var _ inbound.MemoryPositionQuery = (*LiveQueries)(nil)
var _ inbound.MemoryReadScope = (*LiveQueries)(nil)
var _ inbound.LiveMemoryQueries = (*LiveQueries)(nil)

func NewLive(config LiveConfig) (*LiveQueries, error) {
	if config.Mode != LiveCurrent && config.Mode != LiveFrozen {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Metadata, config.Projection, config.History, config.Revisions, config.Repository, config.Evidence} {
		if absent(dependency) {
			return nil, fmt.Errorf("%w: live memory query dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	for _, dependency := range []any{config.Scope, config.Trees, config.Deltas, config.GraphScope, config.GraphGenerations, config.MemoryGenerations, config.PRPins, config.GitPins, config.ProofScope} {
		if dependency != nil && absent(dependency) {
			return nil, fmt.Errorf("%w: declared live memory query dependency is nil", domain.ErrCatalogUnsupported)
		}
	}
	if config.Mode == LiveCurrent && (absent(config.Scope) || absent(config.GraphScope) || config.Delivery == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Mode == LiveCurrent && config.FrozenGeneration != nil {
		return nil, domain.ErrValidation
	}
	var frozen *domain.RepositoryMemorySourceGeneration
	if config.Mode == LiveFrozen {
		if config.FrozenGeneration == nil || config.FrozenGeneration.Validate() != nil || !config.CapturedEvidence {
			return nil, domain.ErrIntegrity
		}
		generation := *config.FrozenGeneration
		frozen = &generation
	}
	return &LiveQueries{
		mode: config.Mode, scope: config.Scope, metadata: config.Metadata, projection: config.Projection,
		history: config.History, revisions: config.Revisions, repository: config.Repository, evidence: config.Evidence,
		trees: config.Trees, deltas: config.Deltas, graphScope: config.GraphScope, graphGenerations: config.GraphGenerations,
		memoryGenerations: config.MemoryGenerations, prPins: config.PRPins, gitPins: config.GitPins,
		delivery: config.Delivery, proofScope: config.ProofScope, frozenGeneration: frozen,
		capturedEvidence: config.CapturedEvidence, requireComplete: config.RequireComplete,
	}, nil
}

func (queries *LiveQueries) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if queries == nil || absent(queries.scope) {
		return domain.ErrCatalogUnsupported
	}
	return queries.scope.WithMemoryReadResources(ctx, repo, read)
}

func readLiveMemory[Result any](ctx context.Context, queries *LiveQueries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		return queries.scope.WithinMemorySnapshot(bound, repo, func(current context.Context) error {
			var err error
			result, err = read(current)
			return err
		})
	})
	if err != nil {
		var zero Result
		return zero, err
	}
	return result, nil
}

func (queries *LiveQueries) memorySourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, *domain.RepositoryMemorySourceGeneration, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, err
	}
	if queries.mode == LiveFrozen {
		generation := queries.frozenGeneration
		if generation == nil || generation.Validate() != nil || generation.Graph.RepoID != repo || !queries.capturedEvidence {
			return domain.RepositoryViewSourceCheckpoint{}, nil, domain.ErrIntegrity
		}
		copy := *generation
		return copy.Graph, &copy, nil
	}
	graph, complete, err := queries.graphSourceGeneration(ctx, repo)
	if err != nil || !complete {
		return graph, nil, err
	}
	if queries.memoryGenerations == nil {
		return graph, nil, nil
	}
	generation, err := queries.memoryGenerations.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if canceled := ctx.Err(); canceled != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, canceled
	}
	if err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, nil, err
	}
	if generation.Validate() != nil || generation.Graph != graph {
		return domain.RepositoryViewSourceCheckpoint{}, nil, domain.ErrIntegrity
	}
	return graph, &generation, nil
}

func validLiveGraphGeneration(repo domain.ContentHash, generation domain.RepositoryViewSourceCheckpoint) bool {
	return generation.RepoID == repo && generation.Scope == domain.RepositoryGraphSourceScope && generation.Validate() == nil
}

func (queries *LiveQueries) graphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, err
	}
	if queries.capturedEvidence {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	if absent(queries.graphScope) {
		return domain.RepositoryViewSourceCheckpoint{}, false, domain.ErrCatalogUnsupported
	}
	if !queries.graphScope.IsPinnedRead(ctx) || queries.graphGenerations == nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	generation, err := queries.graphGenerations.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if canceled := ctx.Err(); canceled != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, canceled
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrCatalogUnsupported) || errors.Is(err, domain.ErrCatalogResetRequired) {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	if err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, err
	}
	if !validLiveGraphGeneration(repo, generation) {
		return domain.RepositoryViewSourceCheckpoint{}, false, domain.ErrIntegrity
	}
	return generation, true, nil
}
