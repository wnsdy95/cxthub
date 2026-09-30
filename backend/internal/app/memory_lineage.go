package app

// The lineage algorithm mirrors cli/internal/app/memorize.go. Shared fixture
// tests pin the wire-level semantics while keeping the modules independent.
import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
)

type memoryProjectionStore interface {
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
	GetMemory(context.Context, domain.ContentHash) (domain.MemoryDigest, error)
}

type memoryProjectionWalker struct {
	ctx            context.Context
	store          memoryProjectionStore
	seen           map[domain.ContentHash]bool
	supplementSeen map[domain.ContentHash]bool
	fingerprinter  *memoryProjectionFingerprinter
	projection     domain.MemoryDigest
	accumulator    *domain.MemoryAccumulator
	found          bool
	complete       bool
	err            error
}

func memoryProjectionFrom(ctx context.Context, store memoryProjectionStore, starts ...domain.ContentHash) (domain.MemoryDigest, bool) {
	digest, found, _, err := memoryProjectionFromDetailed(ctx, store, starts...)
	if err != nil {
		return domain.MemoryDigest{}, false
	}
	return digest, found
}

func memoryProjectionFromDetailed(ctx context.Context, store memoryProjectionStore, starts ...domain.ContentHash) (domain.MemoryDigest, bool, bool, error) {
	walker := memoryProjectionWalker{
		ctx: ctx, store: store,
		seen: map[domain.ContentHash]bool{}, supplementSeen: map[domain.ContentHash]bool{},
		fingerprinter: newMemoryProjectionFingerprinter(ctx, store), complete: true,
		accumulator: domain.NewMemoryAccumulator(maxProjectionMemoryBytes),
	}
	for _, start := range starts {
		walker.walk(start)
	}
	walker.flush()
	return walker.projection, walker.found, walker.complete, walker.err
}

type memoryWalkFrame struct {
	kind uint8
	id   domain.ContentHash
	snap domain.Snapshot // topology only; never retains an ancestor digest.
}

const (
	memoryWalkNode uint8 = iota
	memoryWalkSupplement
	memoryWalkContribution
	memoryWalkOpaque
)
const maxProjectionWalkFrames = 1 << 18

// Explicit post-order actions preserve the old traversal order while releasing
// each inspected archive before descending. A contribution reload is verified
// by the store; no recursive frame keeps every cumulative ancestor alive.
func (w *memoryProjectionWalker) walk(id domain.ContentHash) {
	stack := []memoryWalkFrame{{kind: memoryWalkNode, id: id}}
	push := func(ids []domain.ContentHash, kind uint8) {
		if w.err != nil {
			return
		}
		if len(ids) > maxProjectionWalkFrames-len(stack) {
			w.err = domain.ErrMemoryProjectionLimit
			w.complete = false
			return
		}
		for i := len(ids) - 1; i >= 0; i-- {
			stack = append(stack, memoryWalkFrame{kind: kind, id: ids[i]})
		}
	}
	for len(stack) > 0 && w.err == nil {
		if err := w.ctx.Err(); err != nil {
			w.err = err
			w.complete = false
			return
		}
		if len(stack) > maxProjectionWalkFrames {
			w.err = domain.ErrMemoryProjectionLimit
			w.complete = false
			return
		}
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.kind == memoryWalkContribution || frame.kind == memoryWalkOpaque {
			digest, err := w.store.GetMemory(w.ctx, frame.snap.MemoryHash)
			if err != nil {
				w.complete = false
				w.err = err
				return
			}
			if frame.kind == memoryWalkOpaque {
				w.mergeLegacyOpaque(digest)
			} else if own, ok := w.retainedMemoryContribution(digest, frame.snap); ok {
				w.merge(own)
			}
			continue
		}
		if frame.id == "" || w.seen[frame.id] {
			continue
		}
		if frame.kind == memoryWalkSupplement {
			if w.supplementSeen[frame.id] {
				continue
			}
			w.supplementSeen[frame.id] = true
		} else {
			w.seen[frame.id] = true
		}
		snap, err := w.store.GetSnapshot(w.ctx, frame.id)
		if err != nil {
			w.complete = false
			continue
		}
		if frame.kind == memoryWalkSupplement {
			push(snap.GraftParents, memoryWalkNode)
			push(snap.Parents, memoryWalkSupplement)
			continue
		}
		var digest domain.MemoryDigest
		hasDigest := false
		if snap.MemoryHash != "" {
			digest, err = w.store.GetMemory(w.ctx, snap.MemoryHash)
			if err == nil {
				hasDigest = true
			} else {
				w.complete = false
			}
		}
		if hasDigest && w.memoryDigestCoversLineage(digest, snap) {
			w.merge(digest)
			continue
		}
		if hasDigest && len(digest.Fragments) == 0 {
			digest = domain.MemoryDigest{}
			stack = append(stack, memoryWalkFrame{kind: memoryWalkOpaque, snap: snap})
			push(snap.GraftParents, memoryWalkNode)
			push(snap.Parents, memoryWalkSupplement)
			continue
		}
		digest = domain.MemoryDigest{}
		if hasDigest {
			stack = append(stack, memoryWalkFrame{kind: memoryWalkContribution, snap: snap})
		}
		push(snap.ReachabilityParents(), memoryWalkNode)
	}
}

func (w *memoryProjectionWalker) merge(digest domain.MemoryDigest) {
	if w.err != nil {
		return
	}
	if err := w.accumulator.Add(digest); err != nil {
		w.err = err
		w.complete = false
		return
	}
	w.found = true
}

// mergeLegacyOpaque avoids recreating the historical cumulative-summary
// explosion while repairing old grafts. Narrative replacement is allowed only
// when the later opaque digest byte-for-byte contains every projected fragment
// summary. Structured facts/tasks are still unioned explicitly, so narrative
// containment cannot discard them. No fuzzy or semantic match is inferred.
func (w *memoryProjectionWalker) flush() {
	if w.err != nil || !w.found {
		return
	}
	var err error
	w.projection, err = w.accumulator.Digest()
	if err != nil {
		w.err = err
		w.complete = false
	}
}

func (w *memoryProjectionWalker) mergeLegacyOpaque(digest domain.MemoryDigest) {
	w.flush()
	if w.err != nil {
		return
	}
	if w.found && legacyDigestContainsProjectionNarrative(digest, w.projection) {
		digest.KeyFacts = mergeExactStrings(w.projection.KeyFacts, digest.KeyFacts)
		digest.OpenTasks = mergeExactStrings(w.projection.OpenTasks, digest.OpenTasks)
		w.projection = digest
		if err := w.accumulator.Reset(digest); err != nil {
			w.err = err
			w.complete = false
		}
		return
	}
	w.merge(digest)
}

func legacyDigestContainsProjectionNarrative(legacy, projection domain.MemoryDigest) bool {
	// Narrative containment cannot establish that explicit claims were carried.
	if projection.ClaimsVersion != 0 || projection.HasMemoryClaims() {
		return false
	}
	if len(projection.Fragments) == 0 {
		return projection.Summary == "" || strings.Contains(legacy.Summary, projection.Summary)
	}
	for _, fragment := range projection.Fragments {
		if fragment.Summary != "" && !strings.Contains(legacy.Summary, fragment.Summary) {
			return false
		}
	}
	return true
}

func mergeExactStrings(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		for _, item := range group {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func (w *memoryProjectionWalker) memoryDigestCoversLineage(digest domain.MemoryDigest, snap domain.Snapshot) bool {
	coverage := digest.GraftCoverage
	if coverage == nil || coverage.ProjectionVersion != domain.MemoryProjectionVersion || !coverage.ProjectionComplete {
		// Presence is also the migration proof that the corrected transitive
		// frontier algorithm produced this digest. A no-graft legacy descendant
		// can still have inherited an older hidden graft loss.
		return false
	}
	if coverage.GraftSeq != snap.GraftSeq || len(coverage.GraftParents) != len(snap.GraftParents) {
		return false
	}
	for i := range coverage.GraftParents {
		if coverage.GraftParents[i] != snap.GraftParents[i] {
			return false
		}
	}
	fingerprint, complete := w.fingerprinter.root(snap)
	return complete && coverage.LineageFingerprint == fingerprint
}

type memoryProjectionFingerprintEdge struct {
	SnapshotID domain.ContentHash `json:"snapshot_id"`
	State      domain.ContentHash `json:"state"`
}

type memoryProjectionFingerprintState struct {
	SnapshotID   domain.ContentHash                `json:"snapshot_id"`
	Parents      []memoryProjectionFingerprintEdge `json:"parents"`
	GraftParents []memoryProjectionFingerprintEdge `json:"graft_parents"`
	GraftSeq     uint64                            `json:"graft_seq"`
	MemoryHash   domain.ContentHash                `json:"memory_hash,omitempty"`
}

type memoryProjectionFingerprintResult struct {
	hash domain.ContentHash
	ok   bool
}

type memoryProjectionFingerprinter struct {
	ctx       context.Context
	store     memoryProjectionStore
	memo      map[domain.ContentHash]memoryProjectionFingerprintResult
	visiting  map[domain.ContentHash]bool
	reachable map[domain.ContentHash]bool
}

func newMemoryProjectionFingerprinter(ctx context.Context, store memoryProjectionStore) *memoryProjectionFingerprinter {
	return &memoryProjectionFingerprinter{
		ctx: ctx, store: store,
		memo:     map[domain.ContentHash]memoryProjectionFingerprintResult{},
		visiting: map[domain.ContentHash]bool{}, reachable: map[domain.ContentHash]bool{},
	}
}

// root fingerprints the complete reachable projection state while excluding
// only the root's replaceable MemoryHash. Ancestor MemoryHash values are part
// of the proof, so re-memorizing an ancestor invalidates descendants instead
// of silently shadowing the updated memory.
func (f *memoryProjectionFingerprinter) root(snap domain.Snapshot) (domain.ContentHash, bool) {
	if snap.ID == "" || f.visiting[snap.ID] {
		return "", false
	}
	f.reachable[snap.ID] = true
	f.visiting[snap.ID] = true
	hash, ok := f.snapshot(snap, false)
	delete(f.visiting, snap.ID)
	return hash, ok
}

func (f *memoryProjectionFingerprinter) byID(id domain.ContentHash) (domain.ContentHash, bool) {
	if id != "" {
		// Reachability is a topology fact even when this replica is missing the
		// referenced snapshot body. Coverage remains incomplete, but stale-memory
		// fallback can still distinguish a partial pull from a removed graft.
		f.reachable[id] = true
	}
	if cached, ok := f.memo[id]; ok {
		return cached.hash, cached.ok
	}
	if id == "" || f.visiting[id] {
		return "", false
	}
	snap, err := f.store.GetSnapshot(f.ctx, id)
	if err != nil {
		f.memo[id] = memoryProjectionFingerprintResult{}
		return "", false
	}
	f.reachable[id] = true
	f.visiting[id] = true
	hash, ok := f.snapshot(snap, true)
	delete(f.visiting, id)
	f.memo[id] = memoryProjectionFingerprintResult{hash: hash, ok: ok}
	return hash, ok
}

func (f *memoryProjectionFingerprinter) snapshot(snap domain.Snapshot, includeMemory bool) (domain.ContentHash, bool) {
	if err := domain.ValidateContentHash(snap.ID); err != nil {
		return "", false
	}
	state := memoryProjectionFingerprintState{SnapshotID: snap.ID, GraftSeq: snap.GraftSeq}
	if includeMemory && snap.MemoryHash != "" {
		if err := domain.ValidateContentHash(snap.MemoryHash); err != nil {
			return "", false
		}
		state.MemoryHash = snap.MemoryHash
	}
	appendEdges := func(ids []domain.ContentHash) ([]memoryProjectionFingerprintEdge, bool) {
		edges := make([]memoryProjectionFingerprintEdge, 0, len(ids))
		complete := true
		for _, id := range ids {
			if err := domain.ValidateContentHash(id); err != nil {
				complete = false
				continue
			}
			parentState, ok := f.byID(id)
			if !ok {
				complete = false
				continue
			}
			edges = append(edges, memoryProjectionFingerprintEdge{SnapshotID: id, State: parentState})
		}
		return edges, complete
	}
	var ok bool
	if state.Parents, ok = appendEdges(snap.Parents); !ok {
		return "", false
	}
	if state.GraftParents, ok = appendEdges(snap.GraftParents); !ok {
		return "", false
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", false
	}
	return domain.HashContent(data), true
}

func (w *memoryProjectionWalker) retainedMemoryContribution(digest domain.MemoryDigest, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	if len(digest.Fragments) == 0 {
		return digest, true // Legacy cumulative digest: preserve the opaque value.
	}
	pinned := map[domain.ContentHash]bool{}
	if coverage := digest.GraftCoverage; coverage != nil && coverage.ProjectionVersion == domain.MemoryProjectionVersion {
		for _, source := range coverage.PinnedSources {
			pinned[source] = true
		}
	}
	// Recompute current root membership even when the cheap root-register check
	// already rejected coverage. If a source is still in the graph but this
	// replica cannot read its memory, retain the old fragment as a lossless
	// partial-pull fallback. A source absent from the graph is retained only when
	// it was explicitly imported/pinned; this is what lets graft removal work.
	current := newMemoryProjectionFingerprinter(w.ctx, w.store)
	_, lineageComplete := current.root(snap)
	checkedReadable := map[domain.ContentHash]bool{}
	readable := map[domain.ContentHash]bool{}
	fragments := make([]domain.MemoryFragment, 0, len(digest.Fragments))
	for _, fragment := range digest.Fragments {
		source := fragment.SourceSnapshot
		keep := source == snap.ID || pinned[source]
		if !keep && !lineageComplete {
			// A missing intermediate snapshot hides all of its deeper ancestors.
			// Until the replica is complete we cannot prove that any inherited
			// fragment was removed, so prefer a temporary stale value to data loss.
			keep = true
		} else if !keep && current.reachable[source] {
			if !checkedReadable[source] {
				checkedReadable[source] = true
				readable[source] = memorySourceReproducible(w.ctx, w.store, current.reachable, source)
			}
			keep = !readable[source]
		}
		if keep {
			fragments = append(fragments, fragment)
		}
	}
	if len(fragments) == 0 {
		return domain.MemoryDigest{}, false
	}
	own := domain.MemoryDigest{SnapshotID: snap.ID, Provider: digest.Provider, Fragments: fragments, ClaimsVersion: digest.ClaimsVersion}
	return domain.MergeDigests(domain.MemoryDigest{}, own), true
}

func memorySourceReproducible(
	ctx context.Context,
	store memoryProjectionStore,
	reachable map[domain.ContentHash]bool,
	source domain.ContentHash,
) bool {
	if !reachable[source] {
		return false
	}
	sourceSnapshot, err := store.GetSnapshot(ctx, source)
	if err != nil || sourceSnapshot.MemoryHash == "" {
		return false
	}
	if verified, ok := store.(interface {
		memoryValidated(context.Context, domain.ContentHash) bool
	}); ok {
		return verified.memoryValidated(ctx, sourceSnapshot.MemoryHash)
	}
	_, err = store.GetMemory(ctx, sourceSnapshot.MemoryHash)
	return err == nil
}
