package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// All deliveries use this query. No provider I/O or writes happen in the read
// transaction. Missing evidence is explicit and ordinary observations fill it.
func (s *Service) QueryCodeApplicability(ctx context.Context, repo domain.ContentHash, in domain.CodeSelection) (domain.CodeApplicability, error) {
	if domain.ValidateContentHash(repo) != nil || in.Validate() != nil {
		return domain.CodeApplicability{}, domain.ErrValidation
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.CodeApplicability, error) {
		out := domain.CodeApplicability{Selection: in, Relation: "unknown", Paths: []domain.CodePathState{}}
		r, err := s.meta.GetRepo(ctx, repo)
		if err != nil {
			return out, err
		}
		out.Revision, err = s.RepositoryRevision(ctx, repo)
		if err != nil {
			return out, err
		}
		finish := func(reason string) (domain.CodeApplicability, error) {
			out.Reason = reason
			if len(out.Paths) == 0 {
				for _, p := range in.Paths {
					out.Paths = append(out.Paths, domain.CodePathState{Path: p, State: "unknown", Reason: reason})
				}
			}
			raw, e := json.Marshal(out)
			out.StateHash = domain.HashContent(raw)
			return out, e
		}
		trees, ok := s.meta.(outbound.GitTreeStore)
		deltas, deltaOK := s.meta.(outbound.GitScanStore)
		if !ok || !deltaOK || r.GitRemoteURL == "" {
			return finish("git_evidence_unavailable")
		}
		selected, err := trees.GetGitCommitTree(ctx, repo, r.GitRemoteURL, in.CodeCommit)
		if errors.Is(err, domain.ErrNotFound) {
			return finish("selected_tree_pending")
		}
		if err != nil {
			return out, err
		}
		source, err := trees.GetGitCommitTree(ctx, repo, r.GitRemoteURL, in.SourceCommit)
		if errors.Is(err, domain.ErrNotFound) {
			return finish("source_tree_pending")
		}
		if err != nil {
			return out, err
		}
		if len(source.Parents) > 1 && in.SourceParent == "" {
			return finish("comparison_parent_required")
		}
		if in.SourceParent != "" {
			found := false
			for _, p := range source.Parents {
				found = found || p == in.SourceParent
			}
			if !found {
				return out, domain.ErrValidation
			}
		}
		delta, err := deltas.GetGitDelta(ctx, repo, r.GitRemoteURL, in.SourceCommit, in.SourceParent)
		if errors.Is(err, domain.ErrNotFound) {
			return finish("source_delta_pending")
		}
		if err != nil {
			return out, err
		}
		if !delta.Complete || delta.Commit != in.SourceCommit || !slices.Equal(delta.Parents, source.Parents) {
			return out, domain.ErrIntegrity
		}
		out.Selection.SourceParent = delta.Parent
		var comparison domain.GitCommitTree
		if delta.Parent != "" {
			comparison, err = trees.GetGitCommitTree(ctx, repo, r.GitRemoteURL, delta.Parent)
			if errors.Is(err, domain.ErrNotFound) {
				return finish("comparison_tree_pending")
			}
			if err != nil {
				return out, err
			}
		}
		out.Relation, err = cachedGitRelation(ctx, trees, repo, r.GitRemoteURL, in.SourceCommit, selected)
		if err != nil {
			return out, err
		}
		changes := map[string]domain.GitPathChange{}
		for _, c := range delta.Changes {
			changes[c.Path] = c
		}
		cache := map[string]domain.GitTreeNode{}
		for _, path := range in.Paths {
			c, ok := changes[path]
			if !ok {
				out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "path_not_changed_by_source"})
				continue
			}

			var beforeEntry domain.GitEntry
			if delta.Parent != "" {
				beforeEntry, err = cachedGitEntry(ctx, trees, repo, r.GitRemoteURL, comparison.Tree, path, cache)
				if errors.Is(err, domain.ErrNotFound) {
					out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "comparison_tree_incomplete"})
					continue
				}
				if err != nil {
					return out, err
				}
			}
			if beforeEntry != c.Before {
				return out, domain.ErrIntegrity
			}
			sourceEntry, err := cachedGitEntry(ctx, trees, repo, r.GitRemoteURL, source.Tree, path, cache)
			if errors.Is(err, domain.ErrNotFound) {
				out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "source_tree_incomplete"})
				continue
			}
			if err != nil {
				return out, err
			}
			if sourceEntry != c.After {
				return out, domain.ErrIntegrity
			}
			e, err := cachedGitEntry(ctx, trees, repo, r.GitRemoteURL, selected.Tree, path, cache)
			if errors.Is(err, domain.ErrNotFound) {
				out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "selected_tree_incomplete"})
				continue
			}
			if err != nil {
				return out, err
			}
			out.Paths = append(out.Paths, domain.AssessCodePath(c, e, out.Relation))
		}
		return finish("")
	})
}

// Missing ancestors and the visit bound never become "not an ancestor".
func cachedGitRelation(ctx context.Context, st outbound.GitTreeStore, repo domain.ContentHash, origin, source string, selected domain.GitCommitTree) (string, error) {
	if selected.Commit == source {
		return "ancestor", nil
	}
	seen := map[string]bool{selected.Commit: true}
	todo := append([]string{}, selected.Parents...)
	unknown := false
	for len(todo) > 0 && len(seen) < 4096 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if id == source {
			return "ancestor", nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		c, err := st.GetGitCommitTree(ctx, repo, origin, id)
		if errors.Is(err, domain.ErrNotFound) {
			unknown = true
			continue
		}
		if err != nil {
			return "", err
		}
		todo = append(todo, c.Parents...)
	}
	if unknown || len(todo) > 0 {
		return "unknown", nil
	}
	return "not_ancestor", nil
}
func cachedGitEntry(ctx context.Context, st outbound.GitTreeStore, repo domain.ContentHash, origin, root, path string, cache map[string]domain.GitTreeNode) (domain.GitEntry, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 128 {
		return domain.GitEntry{}, domain.ErrValidation
	}
	id := root
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return domain.GitEntry{}, err
		}
		n, ok := cache[id]
		if !ok {
			var err error
			n, err = st.GetGitTreeNode(ctx, repo, origin, id)
			if err != nil {
				return domain.GitEntry{}, err
			}
			cache[id] = n
		}
		e, ok := n.Entries[part]
		if !ok {
			return domain.GitEntry{}, nil
		}
		if i == len(parts)-1 {
			if e.Mode == "040000" {
				return domain.GitEntry{}, nil
			}
			return e, nil
		}
		if e.Mode != "040000" {
			return domain.GitEntry{}, nil
		}
		id = e.OID
	}
	return domain.GitEntry{}, domain.ErrIntegrity
}
