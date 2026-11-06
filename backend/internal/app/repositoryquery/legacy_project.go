package repositoryquery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func ValidProjectionGraphGeneration(repo domain.ContentHash, generation domain.RepositoryViewSourceCheckpoint) bool {
	return generation.RepoID == repo && generation.Scope == domain.RepositoryGraphSourceScope && generation.Validate() == nil
}

func ProjectionGenerationUnavailable(err error) bool {
	return errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrCatalogUnsupported) || errors.Is(err, domain.ErrCatalogResetRequired)
}

func ReadProjectionGraphSourceGeneration(ctx context.Context, repo domain.ContentHash, scope ProjectionCacheScope, generations GraphGenerationReader, captured bool) (domain.RepositoryViewSourceCheckpoint, bool, error) {
	ctx, span := graphdiagnostics.Start(ctx, graphdiagnostics.SourceGeneration)
	defer span.End()
	if err := ctx.Err(); err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, err
	}
	if captured {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	if absent(scope) {
		return domain.RepositoryViewSourceCheckpoint{}, false, domain.ErrCatalogUnsupported
	}
	if !scope.IsPinnedRead(ctx) || generations == nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	if absent(generations) {
		return domain.RepositoryViewSourceCheckpoint{}, false, domain.ErrCatalogUnsupported
	}
	generation, err := generations.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if canceled := ctx.Err(); canceled != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, canceled
	}
	if ProjectionGenerationUnavailable(err) {
		return domain.RepositoryViewSourceCheckpoint{}, false, nil
	}
	if err != nil {
		return domain.RepositoryViewSourceCheckpoint{}, false, err
	}
	if !ValidProjectionGraphGeneration(repo, generation) {
		return domain.RepositoryViewSourceCheckpoint{}, false, domain.ErrIntegrity
	}
	return generation, true, nil
}

func (projection *LegacyProjection) SourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, bool, error) {
	return ReadProjectionGraphSourceGeneration(ctx, repo, projection.scope, projection.generations, projection.captured)
}

type legacyProjectionContributions struct {
	prs []evidencequery.VerifiedPRContribution
	git []evidencequery.VerifiedGitContribution
}

func (contributions legacyProjectionContributions) Len() int {
	return len(contributions.prs) + len(contributions.git)
}

func (contributions legacyProjectionContributions) At(index int) contextprojection.Contribution {
	if index < len(contributions.prs) {
		verified := contributions.prs[index]
		record := verified.Record
		return contextprojection.Contribution{
			ID: record.ID, DestinationBranchID: record.DestinationBranchID,
			Capture: record.Capture, IntegrationSHA: record.PR.MergeSHA,
			Valid: verified.Valid, SourceBranch: record.PR.HeadBranch, DestinationBranch: record.PR.BaseBranch,
			PRNumber: record.PR.Number, MergedAt: verified.MergedAt,
		}
	}
	verified := contributions.git[index-len(contributions.prs)]
	record := verified.Record
	return contextprojection.Contribution{
		ID: record.ID, DestinationBranchID: record.DestinationBranchID,
		Capture: record.Capture, IntegrationSHA: record.IntegrationSHA,
		Valid: verified.Valid, SourceBranch: verified.SourceBranch, DestinationBranch: verified.DestinationBranch,
	}
}

var _ contextprojection.Contributions = legacyProjectionContributions{}

func projectGraphBase(view domain.RepositoryView, position string) (domain.GraphState, error) {
	graph, err := domain.ProjectGraphState(view, view.DefaultBranch, position)
	if err != nil {
		return graph, err
	}
	graph.BranchContexts = map[string]domain.BranchContext{}
	graph.Integrations = []domain.GraphIntegration{}
	return graph, nil
}

func ProjectGraphPosition(view domain.RepositoryView, position string) (domain.GraphState, error) {
	if position == "" {
		return domain.GraphState{}, domain.ErrValidation
	}
	return projectGraphBase(view, position)
}

func (projection *LegacyProjection) Project(ctx context.Context, view domain.RepositoryView, position string) (domain.GraphState, error) {
	ctx, span := graphdiagnostics.Start(ctx, graphdiagnostics.Projection)
	defer span.End()
	_, base := graphdiagnostics.Start(ctx, graphdiagnostics.GraphBase)
	graph, err := projectGraphBase(view, position)
	base.End()
	if err != nil {
		return graph, err
	}
	if position != "" {
		return graph, nil
	}
	var repo domain.ContentHash
	for _, ref := range view.Refs {
		if ref.Kind == domain.RefBranch {
			repo = ref.RepoID
			break
		}
	}
	if repo == "" {
		return graph, nil
	}
	metadata, err := graphdiagnostics.Read(ctx, graphdiagnostics.Metadata, func(bound context.Context) (domain.Repo, error) {
		return projection.metadata.GetRepo(bound, repo)
	})
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return graph, err
	}
	var evidence contextprojection.CodeEvidence
	if err == nil {
		if metadata.ID != repo {
			return graph, domain.ErrIntegrity
		}
		evidence, err = evidencequery.NewGitEvidence(evidencequery.GitEvidenceConfig{
			RepoID: repo, Origin: metadata.GitRemoteURL, Trees: projection.trees, Deltas: projection.deltas,
			RequireComplete: projection.requireComplete,
		})
		if err != nil {
			return graph, err
		}
	}
	if err := ctx.Err(); err != nil {
		return graph, err
	}
	prs, err := graphdiagnostics.Read(ctx, graphdiagnostics.PREvidence, func(bound context.Context) ([]evidencequery.VerifiedPRContribution, error) {
		return projection.contributions.VerifiedPR(bound, repo, view.History)
	})
	if err != nil {
		return graph, err
	}
	if err := ctx.Err(); err != nil {
		return graph, err
	}
	git, err := graphdiagnostics.Read(ctx, graphdiagnostics.GitEvidence, func(bound context.Context) ([]evidencequery.VerifiedGitContribution, error) {
		return projection.contributions.VerifiedGit(bound, repo, view.History)
	})
	if err != nil {
		return graph, err
	}
	contributions := legacyProjectionContributions{prs: prs, git: git}
	key := BranchProjectionKey{Repo: repo, Graph: view.Revision.Graph, Evidence: view.Revision.Evidence}
	if evidence != nil {
		key.Origin = evidence.Origin()
	}
	generation, cacheable, err := projection.SourceGeneration(ctx, repo)
	if err != nil {
		return graph, err
	}
	key.Generation = generation
	if cacheable {
		if cached, found := projection.branches.Get(key); found {
			graphdiagnostics.RecordCache(ctx, graphdiagnostics.BranchCache, graphdiagnostics.Hit)
			graph.BranchContexts = cached
			for branch, inclusion := range cached {
				graph.BranchSnapshots[branch] = append([]domain.ContentHash{}, inclusion.SnapshotIDs...)
			}
			contextprojection.AppendContributionOperations(&graph, contributions)
			domain.ApplyGraphIntegrations(&graph, view.Snapshots)
			return graph, nil
		}
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.BranchCache, graphdiagnostics.Miss)
	} else {
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.BranchCache, graphdiagnostics.Ineligible)
	}
	ctx, branches := graphdiagnostics.Start(ctx, graphdiagnostics.BranchProjection)
	defer branches.End()
	inputs, err := contextprojection.NewInputs(ctx, repo, view.Snapshots, view.History)
	if err != nil {
		return graph, err
	}
	for _, ref := range view.Refs {
		if ref.Kind != domain.RefBranch || ref.Target == "" {
			continue
		}
		if ref.BranchID == "" {
			ref.BranchID = graph.RefScopes[ref.Name]
		}
		code, err := contextprojection.ResolveBranchCodeFromIndex(ctx, ref, inputs.History(), evidence)
		if err != nil {
			return graph, err
		}
		if evidence == nil {
			code = ""
		}
		plan, err := contextprojection.PrepareBranchContext(ctx, ref, code, inputs, evidence)
		if err != nil {
			return graph, err
		}
		inclusion, err := plan.Finish(ctx, contributions, evidence)
		if err != nil {
			return graph, err
		}
		graph.BranchSnapshots[ref.Name] = inclusion.SnapshotIDs
		graph.BranchContexts[ref.Name] = inclusion
	}
	if cacheable {
		projection.branches.Put(key, graph.BranchContexts)
	}
	contextprojection.AppendContributionOperations(&graph, contributions)
	domain.ApplyGraphIntegrations(&graph, view.Snapshots)
	return graph, nil
}

func (projection *LegacyProjection) Cached(ctx context.Context, repo domain.ContentHash, view domain.RepositoryView, position string) (domain.GraphState, error) {
	ctx, span := graphdiagnostics.Start(ctx, graphdiagnostics.CachedProjection)
	defer span.End()
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	if !projection.scope.IsPinnedRead(ctx) || view.Revision == (domain.RepositoryRevision{}) {
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Ineligible)
		return projection.Admitted(ctx, view, position)
	}
	generation, complete, err := projection.SourceGeneration(ctx, repo)
	if err != nil {
		return domain.GraphState{}, err
	}
	if !complete {
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Ineligible)
		return projection.Admitted(ctx, view, position)
	}
	metadata, err := graphdiagnostics.Read(ctx, graphdiagnostics.Metadata, func(bound context.Context) (domain.Repo, error) {
		return projection.metadata.GetRepo(bound, repo)
	})
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.GraphState{}, err
	}
	fingerprint := sha256.New()
	_, hashing := graphdiagnostics.Start(ctx, graphdiagnostics.Fingerprint)
	err = json.NewEncoder(fingerprint).Encode(view)
	hashing.End()
	if err != nil {
		return domain.GraphState{}, err
	}
	key := GraphProjectionKey{Repo: repo, Revision: view.Revision, Source: generation, Position: position, Origin: metadata.GitRemoteURL}
	copy(key.Inputs[:], fingerprint.Sum(nil))
	if encoded, found := projection.graphs.Get(key); found {
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Hit)
		return graphdiagnostics.Read(ctx, graphdiagnostics.CacheDecode, func(bound context.Context) (domain.GraphState, error) {
			return DecodeGraphProjection(bound, encoded)
		})
	}
	graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Miss)
	_, admission := graphdiagnostics.Start(ctx, graphdiagnostics.BuildAdmission)
	bound, release, err := projection.resources.AcquireBuild(ctx)
	admission.End()
	if err != nil {
		return domain.GraphState{}, err
	}
	defer release()
	_, lookup := graphdiagnostics.Start(ctx, graphdiagnostics.CacheLookup)
	encoded, finish, err := projection.graphs.Lookup(key)
	lookup.End()
	if err != nil {
		return domain.GraphState{}, err
	}
	if finish == nil {
		graphdiagnostics.RecordCache(ctx, graphdiagnostics.GraphCache, graphdiagnostics.Hit)
		return graphdiagnostics.Read(bound, graphdiagnostics.CacheDecode, func(decode context.Context) (domain.GraphState, error) {
			return DecodeGraphProjection(decode, encoded)
		})
	}
	defer finish()
	graph, err := projection.Admitted(bound, view, position)
	if err == nil {
		err = bound.Err()
	}
	if err != nil {
		return domain.GraphState{}, err
	}
	encoded, err = graphdiagnostics.Read(bound, graphdiagnostics.CacheEncode, func(encode context.Context) ([]byte, error) {
		return EncodeGraphProjection(encode, graph)
	})
	if errors.Is(err, ErrGraphProjectionCacheLimit) {
		return graph, bound.Err()
	}
	if err != nil {
		return domain.GraphState{}, err
	}
	if err := bound.Err(); err != nil {
		return domain.GraphState{}, err
	}
	projection.graphs.Put(key, encoded)
	return graph, nil
}

func (projection *LegacyProjection) Admitted(ctx context.Context, view domain.RepositoryView, position string) (domain.GraphState, error) {
	_, admission := graphdiagnostics.Start(ctx, graphdiagnostics.BuildAdmission)
	bound, release, err := projection.resources.AcquireBuild(ctx)
	admission.End()
	if err != nil {
		return domain.GraphState{}, err
	}
	defer release()
	return projection.Project(bound, view, position)
}
