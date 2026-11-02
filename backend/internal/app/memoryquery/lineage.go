package memoryquery

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type LineageReader interface {
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
	GetMemory(context.Context, domain.ContentHash) (domain.MemoryDigest, error)
}

type memoryProjectionWalker struct {
	ctx            context.Context
	store          LineageReader
	seen           map[domain.ContentHash]bool
	supplementSeen map[domain.ContentHash]bool
	fingerprinter  *memoryProjectionFingerprinter
	projection     domain.MemoryDigest
	accumulator    *domain.MemoryAccumulator
	found          bool
	complete       bool
	err            error
}

func ProjectLineage(ctx context.Context, store LineageReader, starts ...domain.ContentHash) (domain.MemoryDigest, bool, bool, error) {
	walker := memoryProjectionWalker{
		ctx: ctx, store: store,
		seen: map[domain.ContentHash]bool{}, supplementSeen: map[domain.ContentHash]bool{},
		fingerprinter: newMemoryProjectionFingerprinter(ctx, store), complete: true,
		accumulator: domain.NewMemoryAccumulator(MaxProjectionMemoryBytes),
	}
	for _, start := range starts {
		walker.walk(start)
	}
	walker.flush()
	return walker.projection, walker.found, walker.complete, walker.err
}

type memoryWalkFrame struct {
	kind       uint8
	snapshotID domain.ContentHash
	snapshot   domain.Snapshot
}

const (
	memoryWalkNode uint8 = iota
	memoryWalkSupplement
	memoryWalkContribution
	memoryWalkOpaque
)
const MaxProjectionWalkFrames = 1 << 18

func (walker *memoryProjectionWalker) walk(snapshotID domain.ContentHash) {
	stack := []memoryWalkFrame{{kind: memoryWalkNode, snapshotID: snapshotID}}
	push := func(ids []domain.ContentHash, kind uint8) {
		if walker.err != nil {
			return
		}
		if len(ids) > MaxProjectionWalkFrames-len(stack) {
			walker.err = domain.ErrMemoryProjectionLimit
			walker.complete = false
			return
		}
		for index := len(ids) - 1; index >= 0; index-- {
			stack = append(stack, memoryWalkFrame{kind: kind, snapshotID: ids[index]})
		}
	}
	for len(stack) > 0 && walker.err == nil {
		if err := walker.ctx.Err(); err != nil {
			walker.err = err
			walker.complete = false
			return
		}
		if len(stack) > MaxProjectionWalkFrames {
			walker.err = domain.ErrMemoryProjectionLimit
			walker.complete = false
			return
		}
		frame := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if frame.kind == memoryWalkContribution || frame.kind == memoryWalkOpaque {
			digest, err := walker.store.GetMemory(walker.ctx, frame.snapshot.MemoryHash)
			if err != nil {
				walker.complete = false
				walker.err = err
				return
			}
			if frame.kind == memoryWalkOpaque {
				walker.mergeLegacyOpaque(digest)
			} else if own, ok := walker.retainedMemoryContribution(digest, frame.snapshot); ok {
				walker.merge(own)
			}
			continue
		}
		if frame.snapshotID == "" || walker.seen[frame.snapshotID] {
			continue
		}
		if frame.kind == memoryWalkSupplement {
			if walker.supplementSeen[frame.snapshotID] {
				continue
			}
			walker.supplementSeen[frame.snapshotID] = true
		} else {
			walker.seen[frame.snapshotID] = true
		}
		snapshot, err := walker.store.GetSnapshot(walker.ctx, frame.snapshotID)
		if err != nil {
			walker.complete = false
			continue
		}
		if frame.kind == memoryWalkSupplement {
			push(snapshot.GraftParents, memoryWalkNode)
			push(snapshot.Parents, memoryWalkSupplement)
			continue
		}
		var digest domain.MemoryDigest
		hasDigest := false
		if snapshot.MemoryHash != "" {
			digest, err = walker.store.GetMemory(walker.ctx, snapshot.MemoryHash)
			if err == nil {
				hasDigest = true
			} else {
				walker.complete = false
			}
		}
		if hasDigest && walker.memoryDigestCoversLineage(digest, snapshot) {
			walker.merge(digest)
			continue
		}
		if hasDigest && len(digest.Fragments) == 0 {
			digest = domain.MemoryDigest{}
			stack = append(stack, memoryWalkFrame{kind: memoryWalkOpaque, snapshot: snapshot})
			push(snapshot.GraftParents, memoryWalkNode)
			push(snapshot.Parents, memoryWalkSupplement)
			continue
		}
		digest = domain.MemoryDigest{}
		if hasDigest {
			stack = append(stack, memoryWalkFrame{kind: memoryWalkContribution, snapshot: snapshot})
		}
		push(snapshot.ReachabilityParents(), memoryWalkNode)
	}
}

func (walker *memoryProjectionWalker) merge(digest domain.MemoryDigest) {
	if walker.err != nil {
		return
	}
	if err := walker.accumulator.Add(digest); err != nil {
		walker.err = err
		walker.complete = false
		return
	}
	walker.found = true
}

func (walker *memoryProjectionWalker) flush() {
	if walker.err != nil || !walker.found {
		return
	}
	var err error
	walker.projection, err = walker.accumulator.Digest()
	if err != nil {
		walker.err = err
		walker.complete = false
	}
}

func (walker *memoryProjectionWalker) mergeLegacyOpaque(digest domain.MemoryDigest) {
	walker.flush()
	if walker.err != nil {
		return
	}
	if walker.found && legacyDigestContainsProjectionNarrative(digest, walker.projection) {
		digest.KeyFacts = mergeExactStrings(walker.projection.KeyFacts, digest.KeyFacts)
		digest.OpenTasks = mergeExactStrings(walker.projection.OpenTasks, digest.OpenTasks)
		walker.projection = digest
		if err := walker.accumulator.Reset(digest); err != nil {
			walker.err = err
			walker.complete = false
		}
		return
	}
	walker.merge(digest)
}

func legacyDigestContainsProjectionNarrative(legacy, projection domain.MemoryDigest) bool {

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
	var merged []string
	for _, group := range groups {
		for _, item := range group {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			merged = append(merged, item)
		}
	}
	return merged
}

func (walker *memoryProjectionWalker) memoryDigestCoversLineage(digest domain.MemoryDigest, snapshot domain.Snapshot) bool {
	coverage := digest.GraftCoverage
	if coverage == nil || coverage.ProjectionVersion != domain.MemoryProjectionVersion || !coverage.ProjectionComplete {

		return false
	}
	if coverage.GraftSeq != snapshot.GraftSeq || len(coverage.GraftParents) != len(snapshot.GraftParents) {
		return false
	}
	for index := range coverage.GraftParents {
		if coverage.GraftParents[index] != snapshot.GraftParents[index] {
			return false
		}
	}
	fingerprint, complete := walker.fingerprinter.root(snapshot)
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
	store     LineageReader
	memo      map[domain.ContentHash]memoryProjectionFingerprintResult
	visiting  map[domain.ContentHash]bool
	reachable map[domain.ContentHash]bool
}

func newMemoryProjectionFingerprinter(ctx context.Context, store LineageReader) *memoryProjectionFingerprinter {
	return &memoryProjectionFingerprinter{
		ctx: ctx, store: store,
		memo:     map[domain.ContentHash]memoryProjectionFingerprintResult{},
		visiting: map[domain.ContentHash]bool{}, reachable: map[domain.ContentHash]bool{},
	}
}

func (fingerprinter *memoryProjectionFingerprinter) root(snapshot domain.Snapshot) (domain.ContentHash, bool) {
	if snapshot.ID == "" || fingerprinter.visiting[snapshot.ID] {
		return "", false
	}
	fingerprinter.reachable[snapshot.ID] = true
	fingerprinter.visiting[snapshot.ID] = true
	hash, ok := fingerprinter.snapshot(snapshot, false)
	delete(fingerprinter.visiting, snapshot.ID)
	return hash, ok
}

func (fingerprinter *memoryProjectionFingerprinter) byID(snapshotID domain.ContentHash) (domain.ContentHash, bool) {
	if snapshotID != "" {

		fingerprinter.reachable[snapshotID] = true
	}
	if cached, ok := fingerprinter.memo[snapshotID]; ok {
		return cached.hash, cached.ok
	}
	if snapshotID == "" || fingerprinter.visiting[snapshotID] {
		return "", false
	}
	snapshot, err := fingerprinter.store.GetSnapshot(fingerprinter.ctx, snapshotID)
	if err != nil {
		fingerprinter.memo[snapshotID] = memoryProjectionFingerprintResult{}
		return "", false
	}
	fingerprinter.reachable[snapshotID] = true
	fingerprinter.visiting[snapshotID] = true
	hash, ok := fingerprinter.snapshot(snapshot, true)
	delete(fingerprinter.visiting, snapshotID)
	fingerprinter.memo[snapshotID] = memoryProjectionFingerprintResult{hash: hash, ok: ok}
	return hash, ok
}

func (fingerprinter *memoryProjectionFingerprinter) snapshot(snapshot domain.Snapshot, includeMemory bool) (domain.ContentHash, bool) {
	if err := domain.ValidateContentHash(snapshot.ID); err != nil {
		return "", false
	}
	state := memoryProjectionFingerprintState{SnapshotID: snapshot.ID, GraftSeq: snapshot.GraftSeq}
	if includeMemory && snapshot.MemoryHash != "" {
		if err := domain.ValidateContentHash(snapshot.MemoryHash); err != nil {
			return "", false
		}
		state.MemoryHash = snapshot.MemoryHash
	}
	appendEdges := func(ids []domain.ContentHash) ([]memoryProjectionFingerprintEdge, bool) {
		edges := make([]memoryProjectionFingerprintEdge, 0, len(ids))
		complete := true
		for _, snapshotID := range ids {
			if err := domain.ValidateContentHash(snapshotID); err != nil {
				complete = false
				continue
			}
			parentState, ok := fingerprinter.byID(snapshotID)
			if !ok {
				complete = false
				continue
			}
			edges = append(edges, memoryProjectionFingerprintEdge{SnapshotID: snapshotID, State: parentState})
		}
		return edges, complete
	}
	var ok bool
	if state.Parents, ok = appendEdges(snapshot.Parents); !ok {
		return "", false
	}
	if state.GraftParents, ok = appendEdges(snapshot.GraftParents); !ok {
		return "", false
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", false
	}
	return domain.HashContent(data), true
}

func (walker *memoryProjectionWalker) retainedMemoryContribution(digest domain.MemoryDigest, snapshot domain.Snapshot) (domain.MemoryDigest, bool) {
	if len(digest.Fragments) == 0 {
		return digest, true
	}
	pinned := map[domain.ContentHash]bool{}
	if coverage := digest.GraftCoverage; coverage != nil && coverage.ProjectionVersion == domain.MemoryProjectionVersion {
		for _, source := range coverage.PinnedSources {
			pinned[source] = true
		}
	}

	current := newMemoryProjectionFingerprinter(walker.ctx, walker.store)
	_, lineageComplete := current.root(snapshot)
	checkedReadable := map[domain.ContentHash]bool{}
	readable := map[domain.ContentHash]bool{}
	fragments := make([]domain.MemoryFragment, 0, len(digest.Fragments))
	for _, fragment := range digest.Fragments {
		source := fragment.SourceSnapshot
		keep := source == snapshot.ID || pinned[source]
		if !keep && !lineageComplete {

			keep = true
		} else if !keep && current.reachable[source] {
			if !checkedReadable[source] {
				checkedReadable[source] = true
				readable[source] = memorySourceReproducible(walker.ctx, walker.store, current.reachable, source)
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
	own := domain.MemoryDigest{SnapshotID: snapshot.ID, Provider: digest.Provider, Fragments: fragments, ClaimsVersion: digest.ClaimsVersion}
	return domain.MergeDigests(domain.MemoryDigest{}, own), true
}

func memorySourceReproducible(
	ctx context.Context,
	store LineageReader,
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
