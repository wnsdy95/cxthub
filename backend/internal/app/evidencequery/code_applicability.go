package evidencequery

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (evidence *GitEvidence) Query(ctx context.Context, in domain.CodeSelection, revision domain.RepositoryRevision) (domain.CodeApplicability, error) {
	out := domain.CodeApplicability{Selection: in, Relation: "unknown", Paths: []domain.CodePathState{}}
	out.Revision = revision
	finish := func(reason string) (domain.CodeApplicability, error) {
		if evidence != nil && evidence.Limited() {
			reason = "evidence_budget_exhausted"
		}
		out.Reason = reason
		if len(out.Paths) == 0 {
			for _, path := range in.Paths {
				out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: reason})
			}
		}
		raw, err := json.Marshal(out)
		out.StateHash = domain.HashContent(raw)
		return out, err
	}
	if evidence == nil || evidence.trees == nil || evidence.deltas == nil || evidence.origin == "" {
		return finish("git_evidence_unavailable")
	}
	trees, deltas := evidence, evidence
	repo, origin := evidence.repo, evidence.origin
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
		for _, parent := range source.Parents {
			found = found || parent == in.SourceParent
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
	out.Relation, err = evidence.Relation(ctx, in.SourceCommit, selected.Commit)
	if err != nil {
		return out, err
	}
	changes := map[string]domain.GitPathChange{}
	for _, change := range delta.Changes {
		changes[change.Path] = change
	}
	cache := map[string]domain.GitTreeNode{}
	for _, path := range in.Paths {
		change, ok := changes[path]
		if !ok {
			out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "path_not_changed_by_source"})
			continue
		}

		var beforeEntry domain.GitEntry
		if delta.Parent != "" {
			beforeEntry, err = CachedGitEntry(ctx, trees, repo, origin, comparison.Tree, path, cache)
			if errors.Is(err, domain.ErrNotFound) {
				out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "comparison_tree_incomplete"})
				continue
			}
			if err != nil {
				return out, err
			}
		}
		if beforeEntry != change.Before {
			return out, domain.ErrIntegrity
		}
		sourceEntry, err := CachedGitEntry(ctx, trees, repo, origin, source.Tree, path, cache)
		if errors.Is(err, domain.ErrNotFound) {
			out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "source_tree_incomplete"})
			continue
		}
		if err != nil {
			return out, err
		}
		if sourceEntry != change.After {
			return out, domain.ErrIntegrity
		}
		selectedEntry, err := CachedGitEntry(ctx, trees, repo, origin, selected.Tree, path, cache)
		if errors.Is(err, domain.ErrNotFound) {
			out.Paths = append(out.Paths, domain.CodePathState{Path: path, State: "unknown", Reason: "selected_tree_incomplete"})
			continue
		}
		if err != nil {
			return out, err
		}
		out.Paths = append(out.Paths, domain.AssessCodePath(change, selectedEntry, out.Relation))
	}
	return finish("")
}

func CachedGitEntry(ctx context.Context, reader GitTreeReader, repo domain.ContentHash, origin, root, path string, cache map[string]domain.GitTreeNode) (domain.GitEntry, error) {
	parts := strings.Split(path, "/")
	if len(parts) > 128 {
		return domain.GitEntry{}, domain.ErrValidation
	}
	identifier := root
	for index, part := range parts {
		if err := ctx.Err(); err != nil {
			return domain.GitEntry{}, err
		}
		node, ok := cache[identifier]
		if !ok {
			var err error
			node, err = reader.GetGitTreeNode(ctx, repo, origin, identifier)
			if err != nil {
				return domain.GitEntry{}, err
			}
			cache[identifier] = node
		}
		entry, ok := node.Entries[part]
		if !ok {
			return domain.GitEntry{}, nil
		}
		if index == len(parts)-1 {
			if entry.Mode == "040000" {
				return domain.GitEntry{}, nil
			}
			return entry, nil
		}
		if entry.Mode != "040000" {
			return domain.GitEntry{}, nil
		}
		identifier = entry.OID
	}
	return domain.GitEntry{}, domain.ErrIntegrity
}
