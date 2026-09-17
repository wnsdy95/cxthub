// Package gitengine provides the outbound.GitEngine adapter (git engine).
//
// It calculates reachability/LCA/ff by following the parents links of the snapshot DAG (data model, sync protocol).
// The main body doc is unnecessary; sufficient is the MetadataStore's snapshot metadata (parents). No external dependencies.
package gitengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Engine is the GitEngine implementation. It determines parent reachability via the MetadataStore.
type Engine struct {
	meta outbound.MetadataStore
}

// NewEngine creates an Engine by injecting the MetadataStore.
func NewEngine(meta outbound.MetadataStore) *Engine { return &Engine{meta: meta} }

var _ outbound.GitEngine = (*Engine)(nil)

// parentsOf returns reachability parents. Missing nodes and failed reads must
// not turn an incomplete graph into an apparently valid root.
// Rules are domain.Snapshot.ReachabilityParents(Parents ∪ GraftParents) — single source of truth — all ancestors
// walk(IsAncestor/MergeBase/AncestorsClosure/ClassifyRefMove) collectively reflect graft reachability.
func (e *Engine) parentsOf(ctx context.Context, repoID, id domain.ContentHash) ([]domain.ContentHash, error) {
	if id == "" {
		return nil, nil
	}
	snap, err := e.meta.GetSnapshot(ctx, repoID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: missing graph snapshot %s: %w", domain.ErrIntegrity, id, err)
		}
		return nil, err
	}
	return snap.ReachabilityParents(), nil
}

// IsAncestor determines if ancestor is an ancestor (or equal to) descendant using BFS.
func (e *Engine) IsAncestor(ctx context.Context, repoID, ancestor, descendant domain.ContentHash) (bool, error) {
	if ancestor == "" {
		return true, nil // Empty ancestor (root previous) is ancestor of everything
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{descendant}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == ancestor {
			return true, nil
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		parents, err := e.parentsOf(ctx, repoID, cur)
		if err != nil {
			return false, err
		}
		queue = append(queue, parents...)
	}
	return false, nil
}

// ancestorsSet returns the set of id itself and all ancestors.
func (e *Engine) ancestorsSet(ctx context.Context, repoID domain.ContentHash, id domain.ContentHash) (map[domain.ContentHash]bool, error) {
	set := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if set[cur] {
			continue
		}
		set[cur] = true
		parents, err := e.parentsOf(ctx, repoID, cur)
		if err != nil {
			return nil, err
		}
		queue = append(queue, parents...)
	}
	return set, nil
}

// MergeBase returns the LCA of two snapshots (b's ancestor that first appears in a's ancestor set). Empty if "".
func (e *Engine) MergeBase(ctx context.Context, repoID, a, b domain.ContentHash) (domain.ContentHash, error) {
	aset, err := e.ancestorsSet(ctx, repoID, a)
	if err != nil {
		return "", err
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if aset[cur] {
			return cur, nil
		}
		parents, err := e.parentsOf(ctx, repoID, cur)
		if err != nil {
			return "", err
		}
		queue = append(queue, parents...)
	}
	return "", nil
}

// AncestorsClosure returns the transitive closure of ids, including ancestors (omitted ancestors included).
func (e *Engine) AncestorsClosure(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) ([]domain.ContentHash, error) {
	set := map[domain.ContentHash]bool{}
	for _, id := range ids {
		ancestors, err := e.ancestorsSet(ctx, repoID, id)
		if err != nil {
			return nil, err
		}
		for h := range ancestors {
			set[h] = true
		}
	}
	out := make([]domain.ContentHash, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	return out, nil
}

// ClassifyRefMove determines the relationship between old and next (sync protocol).
func (e *Engine) ClassifyRefMove(ctx context.Context, repoID, old, next domain.ContentHash) (outbound.RefMoveClass, error) {
	if old == "" {
		return outbound.MoveFastForward, nil // new ref
	}
	if old == next {
		return outbound.MoveUpToDate, nil
	}
	if ff, err := e.IsAncestor(ctx, repoID, old, next); err != nil {
		return "", err
	} else if ff {
		return outbound.MoveFastForward, nil // next is a descendant of old
	}
	if behind, err := e.IsAncestor(ctx, repoID, next, old); err != nil {
		return "", err
	} else if behind {
		return outbound.MoveNonFastForward, nil // next is an ancestor of old (behind)
	}
	return outbound.MoveDiverged, nil
}

// VerifyIntegrity verifies the integrity invariant (sync protocol / data model S-ID/H1).
//
// The server re-hashes the CIR canonical bytes given by the client, which it does not trust.
// Violation → domain.ErrIntegrity.
func (e *Engine) VerifyIntegrity(_ context.Context, snap domain.Snapshot, doc domain.SessionDoc) error {
	if snap.ID == "" || snap.DocHash == "" || doc.Hash == "" {
		return domain.ErrIntegrity
	}
	if snap.ID != snap.DocHash || snap.DocHash != doc.Hash {
		return domain.ErrIntegrity
	}
	cb, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		return domain.ErrIntegrity
	}
	if domain.HashContent(cb) != doc.Hash {
		return domain.ErrIntegrity
	}
	return nil
}
