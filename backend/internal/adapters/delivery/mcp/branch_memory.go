package mcp

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"strings"
)

// Resolve only the user's ref syntax here. Inclusion and code selection belong
// to the read application port, shared with the web and CLI.
func (s *Server) memoryBranch(ctx context.Context, repo domain.Repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "HEAD") {
		ref = defaultBranch(repo)
	}
	if strings.HasPrefix(ref, "sha256:") {
		return "", nil
	}
	refs, err := s.context.ListRefs(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Name == ref && (r.Kind == domain.RefBranch || r.Kind == domain.RefTag) {
			if r.Kind == domain.RefBranch {
				return ref, nil
			}
			return "", nil
		}
	}
	return "", nil
}
func (s *Server) projectMemory(ctx context.Context, repo domain.Repo, a toolArgs, snapshot domain.ContentHash) (domain.MemoryProjection, error) {
	if query, ok := s.context.(inbound.BranchMemoryQuery); ok {
		branch, err := s.memoryBranch(ctx, repo, a.Ref)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		if branch != "" {
			return query.QueryBranchMemory(ctx, repo.ID, branch, snapshot, a.CodeCommit)
		}
	}
	return s.context.GetMemoryProjection(ctx, repo.ID, snapshot)
}

func memoryInclusionSummary(in *domain.BranchContext) map[string]any {
	counts := map[string]int{"included": 0, "not_selected": 0, "review": 0}
	for _, m := range in.Merges {
		counts[m.State]++
	}
	return map[string]any{"branch_id": in.BranchID, "code_commit": in.CodeCommit, "reason": in.Reason, "merges": counts}
}
