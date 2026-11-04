package repositoryquery

import (
	"bytes"
	"context"
	"slices"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type CapturedEvidenceReader interface {
	evidencequery.MetadataReader
	evidencequery.GitTreeReader
	evidencequery.GitDeltaReader
	evidencequery.PRHistoryReader
	evidencequery.PRLedgerReader
	evidencequery.GitLedgerReader
	evidencequery.GitReceiptReader
	evidencequery.GitAdmissionReader
	evidencequery.GitHeadReader
}

func DecodeCapturedGraphInputs(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, entries []domain.RepositoryViewSourceEntry) (domain.Repo, domain.RepositoryView, []domain.RepositoryViewSourceEntry, error) {
	metadata, evidence, err := SplitCapturedGraphInputs(ctx, checkpoint, entries)
	if err != nil {
		return domain.Repo{}, domain.RepositoryView{}, nil, err
	}
	repo, view, err := domain.DecodeRepositoryViewSource(ctx, checkpoint.RepoID, metadata)
	if err != nil {
		return domain.Repo{}, domain.RepositoryView{}, nil, err
	}
	return repo, view, evidence, nil
}

func SplitCapturedGraphInputs(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, entries []domain.RepositoryViewSourceEntry) ([]domain.RepositoryViewSourceEntry, []domain.RepositoryViewSourceEntry, error) {
	if checkpoint.Validate() != nil || checkpoint.Scope != domain.RepositoryGraphSourceScope {
		return nil, nil, domain.ErrIntegrity
	}
	var metadata, evidence []domain.RepositoryViewSourceEntry
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if domain.RepositoryViewSourceKindValid(entry.Kind) {
			metadata = append(metadata, entry)
		} else {
			entry.Value = bytes.Clone(entry.Value)
			evidence = append(evidence, entry)
		}
	}
	return metadata, evidence, nil
}

func ProjectCapturedRepositoryView(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, view domain.RepositoryView, evidence CapturedEvidenceReader, resources GraphResources) (domain.RepositoryView, error) {
	members, err := domain.ProjectSnapshotBranchMemberships(ctx, view.Snapshots, view.Reflog)
	if err != nil {
		return domain.RepositoryView{}, err
	}
	view.Snapshots = slices.Clone(view.Snapshots)
	for index := range view.Snapshots {
		view.Snapshots[index].Branches = nil
		for branch := range members[view.Snapshots[index].ID] {
			view.Snapshots[index].Branches = append(view.Snapshots[index].Branches, branch)
		}
		sort.Strings(view.Snapshots[index].Branches)
	}
	view.Semantics = domain.ProjectContextSemantics(view.Snapshots, view.History)
	view.Refs = legacyGraphNonNil(view.Refs)
	view.Snapshots = legacyGraphNonNil(view.Snapshots)
	view.Reflog = legacyGraphNonNil(view.Reflog)
	view.History = legacyGraphNonNil(view.History)
	view.Pending = legacyGraphNonNil(view.Pending)
	view.Unsync = legacyGraphNonNil(view.Unsync)
	graph, err := ProjectCapturedGraph(ctx, checkpoint, view, evidence, "", resources)
	if err != nil {
		return domain.RepositoryView{}, err
	}
	view.Graph = &graph
	return view, nil
}

func ProjectCapturedGraph(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, view domain.RepositoryView, evidence CapturedEvidenceReader, position string, resources GraphResources) (domain.GraphState, error) {
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	if err := checkpoint.Validate(); err != nil {
		return domain.GraphState{}, err
	}
	if checkpoint.Scope != domain.RepositoryGraphSourceScope {
		return domain.GraphState{}, domain.ErrCatalogResetRequired
	}
	if absent(evidence) {
		return domain.GraphState{}, domain.ErrValidation
	}
	if err := domain.ValidateRepositoryViewScope(ctx, checkpoint.RepoID, view); err != nil {
		return domain.GraphState{}, err
	}
	metadata, err := evidence.GetRepo(ctx, checkpoint.RepoID)
	if err != nil {
		return domain.GraphState{}, err
	}
	if metadata.ID != checkpoint.RepoID || metadata.DefaultBranch != view.DefaultBranch {
		return domain.GraphState{}, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	view.Semantics = domain.ContextSemantics{}
	view.Graph = nil
	projector, err := NewCapturedProjection(struct{ CapturedEvidenceReader }{evidence}, resources)
	if err != nil {
		return domain.GraphState{}, err
	}
	graph, err := projector.Project(ctx, view, position)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return domain.GraphState{}, err
	}
	return graph, nil
}

type capturedProjectionScope struct{}

func (capturedProjectionScope) IsPinnedRead(context.Context) bool {
	return false
}

func NewCapturedProjection(evidence CapturedEvidenceReader, resources GraphResources) (*LegacyProjection, error) {
	contributions, err := evidencequery.New(evidencequery.Config{
		Profile: evidencequery.CompleteEvidence, Metadata: evidence, PRLedger: evidence, PRHistory: evidence,
		GitLedger: evidence, GitHeads: evidence, GitAdmission: evidence, GitReceipts: evidence,
	})
	if err != nil {
		return nil, err
	}
	return NewLegacyProjection(LegacyProjectionConfig{
		Scope: capturedProjectionScope{}, Resources: resources, Metadata: evidence, Contributions: contributions, Trees: evidence, Deltas: evidence,
		Graphs: &GraphCache{}, Branches: &BranchCache{}, Captured: true,
	})
}
