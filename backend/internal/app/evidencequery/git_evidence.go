package evidencequery

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitTreeReader interface {
	GetGitCommitTree(context.Context, domain.ContentHash, string, string) (domain.GitCommitTree, error)
	GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error)
}

type GitDeltaReader interface {
	GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error)
}

type GitEvidenceConfig struct {
	RepoID          domain.ContentHash
	Origin          string
	Trees           GitTreeReader
	Deltas          GitDeltaReader
	RequireComplete bool
}

type gitEvidenceValue[Value any] struct {
	value Value
	err   error
}

type gitEvidenceClosure struct {
	seen       map[string]bool
	expanded   map[string]bool
	todo       []string
	incomplete bool
}

type GitEvidence struct {
	repo             domain.ContentHash
	origin           string
	trees            GitTreeReader
	deltas           GitDeltaReader
	commits          map[string]gitEvidenceValue[domain.GitCommitTree]
	nodes            map[string]gitEvidenceValue[domain.GitTreeNode]
	changes          map[[2]string]gitEvidenceValue[domain.GitCommitDelta]
	closures         map[string]*gitEvidenceClosure
	reads            int
	limited          bool
	traversalLimited bool
	requireComplete  bool
}

func NewGitEvidence(config GitEvidenceConfig) (*GitEvidence, error) {
	for _, reader := range []any{config.Trees, config.Deltas} {
		if reader != nil && absent(reader) {
			return nil, fmt.Errorf("%w: declared Git evidence reader is nil", domain.ErrCatalogUnsupported)
		}
	}
	return &GitEvidence{
		repo:            config.RepoID,
		origin:          config.Origin,
		trees:           config.Trees,
		deltas:          config.Deltas,
		commits:         map[string]gitEvidenceValue[domain.GitCommitTree]{},
		nodes:           map[string]gitEvidenceValue[domain.GitTreeNode]{},
		changes:         map[[2]string]gitEvidenceValue[domain.GitCommitDelta]{},
		closures:        map[string]*gitEvidenceClosure{},
		requireComplete: config.RequireComplete,
	}, nil
}

func (evidence *GitEvidence) Origin() string {
	return evidence.origin
}

func (evidence *GitEvidence) ReadCount() int {
	return evidence.reads
}

func (evidence *GitEvidence) Limited() bool {
	return evidence.limited
}

func (evidence *GitEvidence) TraversalLimited() bool {
	return evidence.traversalLimited
}

func (evidence *GitEvidence) CachedCommit(id string) (domain.GitCommitTree, error, bool) {
	value, found := evidence.commits[id]
	return value.value, value.err, found
}

func (evidence *GitEvidence) ConsumeRead(ctx context.Context, repo domain.ContentHash, origin string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != evidence.repo || origin != evidence.origin {
		return domain.ErrIntegrity
	}
	if evidence.reads >= contextprojection.MaxCodeEvidenceReads {
		evidence.limited = true
		if evidence.requireComplete {
			return domain.ErrMemoryProjectionLimit
		}
		return domain.ErrNotFound
	}
	evidence.reads++
	return nil
}

func (evidence *GitEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitCommitTree, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	if repo != evidence.repo || origin != evidence.origin {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	if value, found := evidence.commits[id]; found {
		return value.value, value.err
	}
	if err := evidence.ConsumeRead(ctx, repo, origin); err != nil {
		return domain.GitCommitTree{}, err
	}
	if evidence.trees == nil {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	value, err := evidence.trees.GetGitCommitTree(ctx, repo, origin, id)
	evidence.commits[id] = gitEvidenceValue[domain.GitCommitTree]{value, err}
	return value, err
}

func (evidence *GitEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitTreeNode, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitTreeNode{}, err
	}
	if repo != evidence.repo || origin != evidence.origin {
		return domain.GitTreeNode{}, domain.ErrIntegrity
	}
	if value, found := evidence.nodes[id]; found {
		return value.value, value.err
	}
	if err := evidence.ConsumeRead(ctx, repo, origin); err != nil {
		return domain.GitTreeNode{}, err
	}
	if evidence.trees == nil {
		return domain.GitTreeNode{}, domain.ErrNotFound
	}
	value, err := evidence.trees.GetGitTreeNode(ctx, repo, origin, id)
	evidence.nodes[id] = gitEvidenceValue[domain.GitTreeNode]{value, err}
	return value, err
}

func (evidence *GitEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, id, parent string) (domain.GitCommitDelta, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if repo != evidence.repo || origin != evidence.origin {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	key := [2]string{id, parent}
	if value, found := evidence.changes[key]; found {
		return value.value, value.err
	}
	if err := evidence.ConsumeRead(ctx, repo, origin); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if evidence.deltas == nil {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	value, err := evidence.deltas.GetGitDelta(ctx, repo, origin, id, parent)
	evidence.changes[key] = gitEvidenceValue[domain.GitCommitDelta]{value, err}
	return value, err
}

func (evidence *GitEvidence) Relation(ctx context.Context, source, selected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	closure := evidence.closures[selected]
	if closure == nil {
		closure = &gitEvidenceClosure{seen: map[string]bool{selected: true}, expanded: map[string]bool{}, todo: []string{selected}}
		evidence.closures[selected] = closure
	}
	if closure.seen[source] {
		return "ancestor", nil
	}
	for len(closure.todo) > 0 && len(closure.expanded) < contextprojection.MaxProjectionSnapshots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := closure.todo[len(closure.todo)-1]
		if closure.expanded[id] {
			closure.todo = closure.todo[:len(closure.todo)-1]
			continue
		}
		commit, err := evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return "", err
		}
		closure.todo = closure.todo[:len(closure.todo)-1]
		closure.expanded[id] = true
		if errors.Is(err, domain.ErrNotFound) {
			closure.incomplete = true
			continue
		}
		for _, parent := range commit.Parents {
			closure.seen[parent] = true
			closure.todo = append(closure.todo, parent)
		}
		if closure.seen[source] {
			return "ancestor", nil
		}
	}
	if closure.incomplete || len(closure.todo) > 0 {
		if len(closure.todo) > 0 {
			if err := evidence.ExhaustTraversal(); err != nil {
				return "", err
			}
		}
		return "unknown", nil
	}
	return "not_ancestor", nil
}

func (evidence *GitEvidence) ExhaustTraversal() error {
	evidence.traversalLimited = true
	if evidence.requireComplete {
		return domain.ErrMemoryProjectionLimit
	}
	return nil
}
