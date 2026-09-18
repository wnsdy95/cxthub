package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const maxCodeEvidenceReads = 8192

type evidenceValue[T any] struct {
	value T
	err   error
}
type evidenceClosure struct {
	seen       map[string]bool
	expanded   map[string]bool
	todo       []string
	incomplete bool
}

// One coherent read owns this cache. Missing immutable evidence is cached too;
// a later request sees its own committed repository generation. No provider I/O.
type codeEvidence struct {
	repo     domain.ContentHash
	origin   string
	trees    outbound.GitTreeStore
	deltas   outbound.GitDeltaReader
	commits  map[string]evidenceValue[domain.GitCommitTree]
	nodes    map[string]evidenceValue[domain.GitTreeNode]
	changes  map[[2]string]evidenceValue[domain.GitCommitDelta]
	closures map[string]*evidenceClosure
	reads    int
	limited  bool
}

func (s *Service) newCodeEvidence(ctx context.Context, repo domain.ContentHash) (*codeEvidence, error) {
	r, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	trees, _ := s.meta.(outbound.GitTreeStore)
	deltas, _ := s.meta.(outbound.GitDeltaReader)
	return &codeEvidence{repo: repo, origin: r.GitRemoteURL, trees: trees, deltas: deltas,
		commits: map[string]evidenceValue[domain.GitCommitTree]{}, nodes: map[string]evidenceValue[domain.GitTreeNode]{}, changes: map[[2]string]evidenceValue[domain.GitCommitDelta]{}, closures: map[string]*evidenceClosure{}}, nil
}
func (e *codeEvidence) allow(ctx context.Context, repo domain.ContentHash, origin string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo != e.repo || origin != e.origin {
		return domain.ErrIntegrity
	}
	if e.reads >= maxCodeEvidenceReads {
		e.limited = true
		return domain.ErrNotFound
	}
	e.reads++
	return nil
}
func (e *codeEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitCommitTree, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	if repo != e.repo || origin != e.origin {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	if v, ok := e.commits[id]; ok {
		return v.value, v.err
	}
	if err := e.allow(ctx, repo, origin); err != nil {
		return domain.GitCommitTree{}, err
	}
	if e.trees == nil {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	v, err := e.trees.GetGitCommitTree(ctx, repo, origin, id)
	e.commits[id] = evidenceValue[domain.GitCommitTree]{v, err}
	return v, err
}
func (e *codeEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitTreeNode, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitTreeNode{}, err
	}
	if repo != e.repo || origin != e.origin {
		return domain.GitTreeNode{}, domain.ErrIntegrity
	}
	if v, ok := e.nodes[id]; ok {
		return v.value, v.err
	}
	if err := e.allow(ctx, repo, origin); err != nil {
		return domain.GitTreeNode{}, err
	}
	if e.trees == nil {
		return domain.GitTreeNode{}, domain.ErrNotFound
	}
	v, err := e.trees.GetGitTreeNode(ctx, repo, origin, id)
	e.nodes[id] = evidenceValue[domain.GitTreeNode]{v, err}
	return v, err
}
func (e *codeEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, id, parent string) (domain.GitCommitDelta, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if repo != e.repo || origin != e.origin {
		return domain.GitCommitDelta{}, domain.ErrIntegrity
	}
	key := [2]string{id, parent}
	if v, ok := e.changes[key]; ok {
		return v.value, v.err
	}
	if err := e.allow(ctx, repo, origin); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if e.deltas == nil {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	v, err := e.deltas.GetGitDelta(ctx, repo, origin, id, parent)
	e.changes[key] = evidenceValue[domain.GitCommitDelta]{v, err}
	return v, err
}

// A proven ancestor returns immediately; later queries resume the same walk.
// This avoids scanning a long history merely to answer a direct-parent query.
func (e *codeEvidence) relation(ctx context.Context, source, selected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	closure := e.closures[selected]
	if closure == nil {
		closure = &evidenceClosure{seen: map[string]bool{selected: true}, expanded: map[string]bool{}, todo: []string{selected}}
		e.closures[selected] = closure
	}
	if closure.seen[source] {
		return "ancestor", nil
	}
	for len(closure.todo) > 0 && len(closure.expanded) < 4096 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id := closure.todo[len(closure.todo)-1]
		closure.todo = closure.todo[:len(closure.todo)-1]
		if closure.expanded[id] {
			continue
		}
		closure.expanded[id] = true
		c, err := e.GetGitCommitTree(ctx, e.repo, e.origin, id)
		if errors.Is(err, domain.ErrNotFound) {
			closure.incomplete = true
			continue
		}
		if err != nil {
			return "", err
		}
		for _, p := range c.Parents {
			closure.seen[p] = true
			closure.todo = append(closure.todo, p)
		}
		if closure.seen[source] {
			return "ancestor", nil
		}
	}
	if closure.incomplete || len(closure.todo) > 0 {
		return "unknown", nil
	}
	return "not_ancestor", nil
}
