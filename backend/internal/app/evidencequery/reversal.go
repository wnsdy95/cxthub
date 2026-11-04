package evidencequery

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ReversalReader interface {
	ReadCommitDelta(context.Context, string, string, string) (domain.GitCommitDelta, error)
	IsGitAncestor(context.Context, string, string, string) (bool, error)
}

func VerifyReversal(ctx context.Context, reader ReversalReader, origin, target, targetParent, candidate, candidateParent string) (domain.GitReversalEvidence, error) {
	var empty domain.GitReversalEvidence
	if absent(reader) {
		return empty, fmt.Errorf("Git evidence reader unavailable")
	}
	before, err := reader.ReadCommitDelta(ctx, origin, target, targetParent)
	if err != nil {
		return empty, err
	}
	after, err := reader.ReadCommitDelta(ctx, origin, candidate, candidateParent)
	if err != nil {
		return empty, err
	}
	if before.Commit != target || after.Commit != candidate || (targetParent != "" && before.Parent != targetParent) || (candidateParent != "" && after.Parent != candidateParent) {
		return empty, domain.ErrIntegrity
	}
	if err := before.Validate(); err != nil {
		return empty, err
	}
	if err := after.Validate(); err != nil {
		return empty, err
	}
	if after.Parent == "" {
		return domain.AssessGitReversal(before, after)
	}
	ancestor, err := reader.IsGitAncestor(ctx, origin, target, after.Parent)
	if err != nil {
		return empty, err
	}
	if !ancestor {
		return domain.GitReversalEvidence{Commit: candidate, Target: target, Parent: after.Parent, TargetParent: before.Parent, Coverage: "unverified", Reason: "target_not_in_candidate_ancestry", Paths: []string{}, UnverifiedPaths: []string{}, OtherPaths: []string{}}, nil
	}
	return domain.AssessGitReversal(before, after)
}

type CachedReversalConfig struct {
	RepoID   domain.ContentHash
	Cache    GitDeltaReader
	Provider ReversalReader
}

type CachedReversalReader struct {
	repo     domain.ContentHash
	cache    GitDeltaReader
	provider ReversalReader
}

func NewCachedReversalReader(config CachedReversalConfig) (*CachedReversalReader, error) {
	if absent(config.Cache) || absent(config.Provider) {
		return nil, fmt.Errorf("%w: cached reversal readers are required", domain.ErrCatalogUnsupported)
	}
	return &CachedReversalReader{repo: config.RepoID, cache: config.Cache, provider: config.Provider}, nil
}

func (reader *CachedReversalReader) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	delta, err := reader.cache.GetGitDelta(ctx, reader.repo, origin, commit, parent)
	if err == nil {
		return delta, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return delta, err
	}
	return reader.provider.ReadCommitDelta(ctx, origin, commit, parent)
}

func (reader *CachedReversalReader) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	todo := []string{descendant}
	seen := map[string]bool{}
	unknown := false
	for len(todo) > 0 && len(seen) < 256 {
		commit := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if commit == ancestor {
			return true, nil
		}
		if seen[commit] {
			continue
		}
		seen[commit] = true
		delta, err := reader.cache.GetGitDelta(ctx, reader.repo, origin, commit, "")
		if errors.Is(err, domain.ErrNotFound) {
			unknown = true
			continue
		}
		if err != nil {
			return false, err
		}
		todo = append(todo, delta.Parents...)
	}
	if len(todo) == 0 && !unknown {
		return false, nil
	}
	return reader.provider.IsGitAncestor(ctx, origin, ancestor, descendant)
}
