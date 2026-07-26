// Package gitengine provides the outbound.GitEngine adapter (git engine).
//
// It calculates reachability/LCA/ff by following the parents links of the snapshot DAG (DATA-MODEL §2.5, SYNC-PROTOCOL §5.2).
// The main body doc is unnecessary; sufficient is the MetadataStore's snapshot metadata (parents). No external dependencies.
package gitengine

import (
	"context"

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

// parentsOf returns the reachability parent list of a snapshot (empty slice if non-existent).
// Rules are domain.Snapshot.ReachabilityParents(Parents ∪ GraftParents) — single source of truth — all ancestors
// walk(IsAncestor/MergeBase/AncestorsClosure/ClassifyRefMove) collectively reflect graft reachability.
func (e *Engine) parentsOf(ctx context.Context, repoID, id domain.ContentHash) []domain.ContentHash {
	snap, err := e.meta.GetSnapshot(ctx, repoID, id)
	if err != nil {
		return nil
	}
	return snap.ReachabilityParents()
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
		queue = append(queue, e.parentsOf(ctx, repoID, cur)...)
	}
	return false, nil
}

// ancestorsSet returns the set of id itself and all ancestors.
func (e *Engine) ancestorsSet(ctx context.Context, repoID domain.ContentHash, id domain.ContentHash) map[domain.ContentHash]bool {
	set := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if set[cur] {
			continue
		}
		set[cur] = true
		queue = append(queue, e.parentsOf(ctx, repoID, cur)...)
	}
	return set
}

// MergeBase returns the LCA of two snapshots (b's ancestor that first appears in a's ancestor set). Empty if "".
func (e *Engine) MergeBase(ctx context.Context, repoID, a, b domain.ContentHash) (domain.ContentHash, error) {
	aset := e.ancestorsSet(ctx, repoID, a)
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
		queue = append(queue, e.parentsOf(ctx, repoID, cur)...)
	}
	return "", nil
}

// AncestorsClosure returns the transitive closure of ids, including ancestors (omitted ancestors included).
func (e *Engine) AncestorsClosure(ctx context.Context, repoID domain.ContentHash, ids []domain.ContentHash) ([]domain.ContentHash, error) {
	set := map[domain.ContentHash]bool{}
	for _, id := range ids {
		for h := range e.ancestorsSet(ctx, repoID, id) {
			set[h] = true
		}
	}
	out := make([]domain.ContentHash, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	return out, nil
}

// ClassifyRefMove determines the relationship between old and next (SYNC-PROTOCOL §5.1).
func (e *Engine) ClassifyRefMove(ctx context.Context, repoID, old, next domain.ContentHash) (outbound.RefMoveClass, error) {
	if old == "" {
		return outbound.MoveFastForward, nil // new ref
	}
	if old == next {
		return outbound.MoveUpToDate, nil
	}
	if ff, _ := e.IsAncestor(ctx, repoID, old, next); ff {
		return outbound.MoveFastForward, nil // next is a descendant of old
	}
	if behind, _ := e.IsAncestor(ctx, repoID, next, old); behind {
		return outbound.MoveNonFastForward, nil // next is an ancestor of old (behind)
	}
	return outbound.MoveDiverged, nil
}

// VerifyIntegrity verifies the integrity invariant (SYNC-PROTOCOL §3.4 / DATA-MODEL S-ID/H1).
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
