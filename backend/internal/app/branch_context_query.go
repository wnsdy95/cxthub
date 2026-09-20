package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// branchCode selects only exact associations with this context and identity.
// A shared content hash may have different source and merged code associations;
// branch identity disambiguates them without guessing from wall-clock order.
func branchCode(ref domain.Ref, history []domain.HistoryEvent) string {
	codes := map[string]bool{}
	mergedSources := map[string]bool{}
	for _, h := range history {
		if h.BranchID != ref.BranchID || h.Target != ref.Target {
			continue
		}
		code := ""
		switch h.Kind {
		case "publish":
			if h.Source == h.Target {
				code = h.GitAfter
			}
		case "position", "advance", "birth", "attach":
			code = h.GitAfter
		case "pr-merge":
			if h.PRCompleted && h.PR != nil {
				code = h.PR.MergeSHA
				// Tracking aliases share the destination identity. Their exact
				// source publication still names the pre-squash Git head. A
				// completed receipt proves that this same context was integrated
				// at MergeSHA, so HeadSHA is not a competing destination position.
				// Unrelated positions and competing merges remain ambiguous.
				if h.Source == ref.Target && h.PR.HeadSHA != code && domain.ValidateGitOID(code) == nil {
					mergedSources[h.PR.HeadSHA] = true
				}
			}
		}
		if domain.ValidateGitOID(code) == nil {
			codes[code] = true
		}
	}
	for code := range mergedSources {
		delete(codes, code)
	}
	if len(codes) == 1 {
		for code := range codes {
			return code
		}
	}
	return ""
}

func (s *Service) QueryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || domain.ValidateBranchName(branch) != nil || (code != "" && domain.ValidateGitOID(code) != nil) {
		return domain.MemoryProjection{}, domain.ErrValidation
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.MemoryProjection, error) {
		return s.queryBranchMemory(ctx, repo, branch, snapshot, code)
	})
}
func (s *Service) queryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	ref, err := s.meta.GetRef(ctx, repo, domain.RefBranch, branch)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if ref.BranchID == "" {
		bindings, err := domain.ProjectContextBranches(history)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		ref.BranchID = bindings.Identity(string(repo), branch)
	}
	ref.Target = snapshot
	if code == "" {
		code = branchCode(ref, history)
	}
	snaps, err := s.meta.ListSnapshots(ctx, repo, "")
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	evidence, err := s.newCodeEvidence(ctx, repo)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	inclusion, err := s.branchContext(ctx, ref, code, snaps, history, evidence)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return s.projectBranchMemory(ctx, repo, inclusion, snaps)
}

// Both full and pending views use this same projection. No adapter enriches a
// graph independently; one MVCC generation owns code, receipts and contexts.
func (s *Service) projectGraphState(ctx context.Context, v domain.RepositoryView, position string) (domain.GraphState, error) {
	graph, err := domain.ProjectGraphState(v, v.DefaultBranch, position)
	if err != nil {
		return graph, err
	}
	graph.BranchContexts = map[string]domain.BranchContext{}
	graph.Integrations = []domain.GraphIntegration{}
	// An explicit worktree position retains its existing historical scope.
	if position != "" {
		return graph, nil
	}
	var repo domain.ContentHash
	for _, r := range v.Refs {
		if r.Kind == domain.RefBranch {
			repo = r.RepoID
			break
		}
	}
	if repo == "" {
		return graph, nil
	}
	evidence, err := s.newCodeEvidence(ctx, repo)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return graph, err
	}
	for _, ref := range v.Refs {
		if ref.Kind != domain.RefBranch || ref.Target == "" {
			continue
		}
		if ref.BranchID == "" {
			ref.BranchID = graph.RefScopes[ref.Name]
		}
		code := branchCode(ref, v.History)
		if evidence == nil {
			code = ""
		} // legacy metadata without a bound Git origin
		inclusion, err := s.branchContext(ctx, ref, code, v.Snapshots, v.History, evidence)
		if err != nil {
			return graph, err
		}
		graph.BranchSnapshots[ref.Name] = inclusion.SnapshotIDs
		graph.BranchContexts[ref.Name] = inclusion
	}
	domain.ApplyGraphIntegrations(&graph, v.Snapshots)
	return graph, nil
}
