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
		evidence, err := s.newCodeEvidence(ctx, repo)
		if err != nil {
			return domain.CodeApplicability{}, err
		}
		revision, err := s.RepositoryRevision(ctx, repo)
		if err != nil {
			return domain.CodeApplicability{}, err
		}
		return evidence.query(ctx, in, revision)
	})
}

func (evidence *codeEvidence) query(ctx context.Context, in domain.CodeSelection, revision domain.RepositoryRevision) (domain.CodeApplicability, error) {
	out := domain.CodeApplicability{Selection: in, Relation: "unknown", Paths: []domain.CodePathState{}}
	out.Revision = revision
	finish := func(reason string) (domain.CodeApplicability, error) {
		if evidence.limited {
			reason = "evidence_budget_exhausted"
		}
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
	trees, deltas := evidence, evidence
	repo, origin := evidence.repo, evidence.origin
	if evidence.trees == nil || evidence.deltas == nil || origin == "" {
		return finish("git_evidence_unavailable")
	}
	selected, err := trees.GetGitCommitTree(ctx, repo, origin, in.CodeCommit)
	if errors.Is(err, domain.ErrNotFound) {
		return finish("selected_tree_pending")
	}
	if err != nil {
		return out, err
	}
	source, err := trees.GetGitCommitTree(ctx, repo, origin, in.SourceCommit)
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
	delta, err := deltas.GetGitDelta(ctx, repo, origin, in.SourceCommit, in.SourceParent)
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
		comparison, err = trees.GetGitCommitTree(ctx, repo, origin, delta.Parent)
		if errors.Is(err, domain.ErrNotFound) {
			return finish("comparison_tree_pending")
		}
		if err != nil {
			return out, err
		}
	}
	out.Relation, err = evidence.relation(ctx, in.SourceCommit, selected.Commit)
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
			beforeEntry, err = cachedGitEntry(ctx, trees, repo, origin, comparison.Tree, path, cache)
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
		sourceEntry, err := cachedGitEntry(ctx, trees, repo, origin, source.Tree, path, cache)
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
		e, err := cachedGitEntry(ctx, trees, repo, origin, selected.Tree, path, cache)
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
