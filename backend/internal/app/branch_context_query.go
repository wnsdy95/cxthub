package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"sort"
)

// branchCode selects only exact associations with this context and identity.
// A shared content hash may have different source and merged code associations;
// branch identity disambiguates them without guessing from wall-clock order.
func branchCode(ref domain.Ref, history []domain.HistoryEvent) string {
	codes := branchCodeCandidates(ref, history)
	if len(codes) == 1 {
		for code := range codes {
			return code
		}
	}
	return ""
}

func branchCodeCandidates(ref domain.Ref, history []domain.HistoryEvent) map[string]bool {
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
	return codes
}

// A completed append moves the context pointer, but its arrival time does not
// select a Git revision. Follow only the recorded pre-append selections, then
// choose a revision proven to contain every candidate. Explicit code requests
// bypass this resolver. Ordinary conflicting observations remain ambiguous.
func resolvedBranchCode(ctx context.Context, ref domain.Ref, history []domain.HistoryEvent, evidence *codeEvidence) (string, error) {
	if evidence == nil {
		return branchCode(ref, history), nil
	}
	byTarget := map[domain.ContentHash][]domain.HistoryEvent{}
	for _, h := range history {
		if h.BranchID == ref.BranchID {
			byTarget[h.Target] = append(byTarget[h.Target], h)
		}
	}
	candidates := map[string]bool{}
	visited, active := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	var collect func(domain.ContentHash) error
	collect = func(target domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if active[target] {
			return errAmbiguousBranchCode
		}
		if visited[target] {
			return nil
		}
		if len(visited) >= maxCodeEvidenceReads {
			return errAmbiguousBranchCode
		}
		visited[target], active[target] = true, true
		defer delete(active, target)
		r := ref
		r.Target = target
		rows := byTarget[target]
		codes := branchCodeCandidates(r, rows)
		merges := map[string]bool{}
		for _, h := range rows {
			if h.Kind == "pr-merge" && h.PRCompleted && h.PR != nil && domain.ValidateGitOID(h.PR.MergeSHA) == nil {
				merges[h.PR.MergeSHA] = true
			}
		}
		for code := range codes {
			if target == ref.Target && len(merges) != 0 && !merges[code] {
				return errAmbiguousBranchCode
			}
			candidates[code] = true
		}
		if len(merges) == 0 {
			if target == ref.Target && len(codes) > 1 {
				return errAmbiguousBranchCode
			}
			return nil
		}
		for _, h := range rows {
			if h.Kind == "pr-merge" && h.PRCompleted && h.PR != nil && h.SharedTarget != "" && h.SharedTarget != target {
				if err := collect(h.SharedTarget); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(ref.Target); err != nil {
		if errors.Is(err, errAmbiguousBranchCode) {
			return "", nil
		}
		return "", err
	}
	codes := make([]string, 0, len(candidates))
	for c := range candidates {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	if len(codes) == 0 {
		return "", nil
	}
	selected := codes[0]
	for _, c := range codes[1:] {
		relation, err := evidence.relation(ctx, selected, c)
		if err != nil {
			return "", err
		}
		if relation == "ancestor" {
			selected = c
		}
	}
	for _, c := range codes {
		relation, err := evidence.relation(ctx, c, selected)
		if err != nil {
			return "", err
		}
		if relation != "ancestor" {
			return "", nil
		}
	}
	return selected, nil
}

var errAmbiguousBranchCode = errors.New("ambiguous ordinary branch code observations")

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
	snaps, err := s.meta.ListSnapshots(ctx, repo, "")
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	evidence, err := s.newCodeEvidence(ctx, repo)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if code == "" {
		code, err = resolvedBranchCode(ctx, ref, history, evidence)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
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
	// Only committed graph/evidence generations can reuse derived branch facts.
	// Pending captures still rebuild their current display projection below.
	key := branchProjectionKey{repo: repo, graph: v.Revision.Graph, evidence: v.Revision.Evidence}
	if evidence != nil {
		key.origin = evidence.origin
	}
	_, insideWrite := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	cacheable := v.Revision.Graph > 0 && !insideWrite
	if cacheable {
		if cached, ok := s.branchCache.get(key); ok {
			graph.BranchContexts = cached
			for branch, c := range cached {
				graph.BranchSnapshots[branch] = append([]domain.ContentHash{}, c.SnapshotIDs...)
			}
			domain.ApplyGraphIntegrations(&graph, v.Snapshots)
			return graph, nil
		}
	}
	for _, ref := range v.Refs {
		if ref.Kind != domain.RefBranch || ref.Target == "" {
			continue
		}
		if ref.BranchID == "" {
			ref.BranchID = graph.RefScopes[ref.Name]
		}
		code, err := resolvedBranchCode(ctx, ref, v.History, evidence)
		if err != nil {
			return graph, err
		}
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
	if cacheable {
		s.branchCache.put(key, graph.BranchContexts)
	}
	domain.ApplyGraphIntegrations(&graph, v.Snapshots)
	return graph, nil
}
