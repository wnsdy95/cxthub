package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// MemorizeService implements the Memorize inbound port as per the compatibility rules.
//
// Dependent outbound ports: GitContext, CaptureSource (registry), ProviderCodec (registry),
// MemorySource (registry), MemoryDistiller, SessionStore.
//
// Memorize sequence:
//  1. GitContext.CurrentRepo/CurrentBranch(Cwd) → repoID, branch confirmed.
//  2. CaptureSource[Provider].LocateActiveSession+ReadSession → raw session.
//  3. ProviderCodec[Provider].Decode(raw) → CIRDocument.
//  4. MemorySource[Provider].ReadNative(Cwd, SessionOriginID) → (native, found). Native priority.
//  5. MemoryDistiller.Distill(cir, native) → MemoryDigest (native==nil fallback to CIR distillation).
//  6. SessionStore.PutMemory(digest) → MemoryHash.
//  7. Attach MemoryHash to current branch HEAD snapshot (snapshot holds raw+memory, invariant 1).
//  8. Return MemorizeOutput{SnapshotID, MemoryHash, Attached}.
type MemorizeService struct {
	gitCtx     outbound.GitContext
	captures   map[domain.ProviderKind]outbound.CaptureSource
	codecs     map[domain.ProviderKind]outbound.ProviderCodec
	memSources map[domain.ProviderKind]outbound.MemorySource
	distiller  outbound.MemoryDistiller
	store      outbound.SessionStore
}

// NewMemorizeService creates and injects dependencies for MemorizeService.
func NewMemorizeService(
	gitCtx outbound.GitContext,
	captures map[domain.ProviderKind]outbound.CaptureSource,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	memSources map[domain.ProviderKind]outbound.MemorySource,
	distiller outbound.MemoryDistiller,
	store outbound.SessionStore,
) *MemorizeService {
	return &MemorizeService{
		gitCtx:     gitCtx,
		captures:   captures,
		codecs:     codecs,
		memSources: memSources,
		distiller:  distiller,
		store:      store,
	}
}

// Memorize empirically verifies the context of the current branch head snapshot to create a MemoryDigest and attach it.
//
// Sequence: head snapshot/doc retrieval → absorb native memory (if present) → RuleDistiller distillation (deterministic)
// → PutMemory(content-addressed) → attach MemoryHash to head snapshot metadata.
// Push carries the attached memory with the raw document (compatibility rules).
func (s *MemorizeService) Memorize(ctx context.Context, in inbound.MemorizeInput) (inbound.MemorizeOutput, error) {
	provider := in.Provider
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	target := domain.ContentHash("")
	if in.Ref != "" {
		// Explicit target (snapshotID/branch) — checkpoint-like for when head has already moved.
		id, rerr := resolveRef(ctx, s.store, string(repo.ID), in.Ref)
		if rerr != nil {
			return inbound.MemorizeOutput{}, rerr
		}
		target = id
	} else {
		branch, _ := s.gitCtx.CurrentBranch(ctx, in.Cwd)
		if branch == "" || branch == "HEAD" {
			branch = repo.DefaultBranch
		}
		ref, rerr := s.store.GetRef(ctx, string(repo.ID), domain.RefBranch, branch)
		if rerr != nil || ref.Target == "" {
			return inbound.MemorizeOutput{}, domain.ErrNotFound // need commit to create snapshot
		}
		target = ref.Target
	}
	snap, err := s.store.GetSnapshot(ctx, target)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	doc, err := s.store.GetDoc(ctx, snap.DocHash)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	sourceProvider := doc.CIR.Envelope.SourceProvider
	if sourceProvider != "" {
		if provider != "" && provider != sourceProvider {
			return inbound.MemorizeOutput{}, fmt.Errorf("snapshot provider %q does not match requested memory provider %q", sourceProvider, provider)
		}
		provider = sourceProvider
	} else if provider == "" {
		// Legacy documents predate SourceProvider. Preserve the original default
		// only when the snapshot itself cannot identify its native-memory source.
		provider = domain.ProviderClaude
	}

	// Absorb native memory (e.g., CLAUDE.md/MEMORY.md) first — fallback to CIR distillation if not present.
	var nativePtr *domain.NativeMemory
	if src, ok := s.memSources[provider]; ok {
		if native, found, nerr := src.ReadNative(ctx, in.Cwd, doc.CIR.Envelope.SessionOriginID); nerr == nil && found {
			nativePtr = &native
		}
	}
	// Distill what the provider can currently see. The complete pre-compaction
	// transcript stays archived in the immutable doc, but feeding it back into
	// memory would resurrect context the provider intentionally replaced.
	digest, err := s.distiller.Distill(ctx, doc.CIR.EffectiveContext(), nativePtr)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	digest.SnapshotID = snap.ID
	// Every coverage-bearing digest needs snapshot-scoped provenance even when
	// it has no inherited memory. Otherwise a future lineage mutation cannot
	// separate this snapshot's own contribution from opaque inherited content.
	digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	// Project memory follows every natural/graft lineage and unions provenance
	// fragments; choosing one globally closest digest loses sibling PR memory.
	// Filter noisy prior KeyFacts so tool names and ingestion markers do not propagate forever across generations. Keep only sentence-form facts, using the same rules as the seed filter.
	projectionState, prior, hasPrior, priorComplete, err := stablePriorMemoryProjection(ctx, s.store, snap.ID)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	snap = projectionState.snap
	if hasPrior {
		prior.KeyFacts = seedWorthyFacts(prior.KeyFacts)
		prior = boundCarriedDigest(prior)
		digest = domain.MergeDigests(prior, digest)
	}
	// Coverage is derived after the final fragment union. This records imports
	// such as branch-seed main memory that current graph parents cannot replay;
	// computing it before the merge would make those fragments disappear on the
	// first later graft mutation.
	digest.GraftCoverage = memoryGraftCoverageFromState(ctx, s.store, projectionState, digest.Fragments, priorComplete)
	if snap.MemoryHash != "" {
		current, err := s.store.GetMemory(ctx, snap.MemoryHash)
		if err != nil {
			// A causal child cannot safely point at a missing parent. Pull/fsck must
			// restore or diagnose it before another attachment is created.
			return inbound.MemorizeOutput{}, err
		}
		if sameMemoryDigestPayload(current, digest) {
			// Verify that another terminal did not move the pointer between the
			// stable projection and this no-op decision.
			if err := s.store.CompareAndSwapSnapshotMemory(ctx, snap.ID, snap.MemoryHash, snap.MemoryHash); err != nil {
				return inbound.MemorizeOutput{}, err
			}
			return inbound.MemorizeOutput{SnapshotID: snap.ID, MemoryHash: snap.MemoryHash, Attached: true}, nil
		}
	}
	// Memory attachments form a snapshot-scoped causal chain. The projection
	// may absorb many graph lineages, but only the previously attached digest on
	// this exact snapshot is the mutable-ref parent.
	digest.PreviousMemoryHash = snap.MemoryHash
	memHash, err := s.store.PutMemory(ctx, digest)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	// A second terminal may have memorized the same snapshot while distillation
	// was running. CAS keeps both immutable blobs and rejects the stale pointer
	// move instead of making the last filesystem rename win.
	if err := s.store.CompareAndSwapSnapshotMemory(ctx, snap.ID, digest.PreviousMemoryHash, memHash); err != nil {
		return inbound.MemorizeOutput{}, err
	}
	return inbound.MemorizeOutput{SnapshotID: snap.ID, MemoryHash: memHash, Attached: true}, nil
}

func sameMemoryDigestPayload(left, right domain.MemoryDigest) bool {
	left.PreviousMemoryHash = ""
	right.PreviousMemoryHash = ""
	leftHash, leftErr := domain.MemoryDigestHash(left)
	rightHash, rightErr := domain.MemoryDigestHash(right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}

// Ensure MemorizeService implements inbound.Memorize.
var _ inbound.Memorize = (*MemorizeService)(nil)

// ancestorMemoryProjection projects every nearest memory frontier behind the
// snapshot. A digest is a valid traversal stop only when it proves coverage of
// the memory-bearing snapshot's current mutable graft register.
func ancestorMemoryProjection(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	digest, found, _ := ancestorMemoryProjectionDetailed(ctx, store, snap)
	return digest, found
}

func ancestorMemoryProjectionDetailed(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool, bool) {
	return memoryProjectionFromDetailed(ctx, store, snap.ReachabilityParents()...)
}

// snapshotMemoryProjection resolves a snapshot's own digest together with any
// graft lineages added or superseded after that digest was attached.
func snapshotMemoryProjection(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	digest, found, _ := snapshotMemoryProjectionDetailed(ctx, store, snap)
	return digest, found
}

func snapshotMemoryProjectionDetailed(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool, bool) {
	return memoryProjectionFromDetailed(ctx, store, snap.ID)
}

// priorMemoryProjection returns everything a fresh distillation of snap must
// inherit while replacing that snapshot's previous own contribution. Modern
// digests can also own pinned imports that are not reachable from parents, so
// reading ancestors alone is lossy when the same seed/head is memorized again.
// Legacy cumulative digests have no provenance to separate own from inherited
// content and retain the historical ancestor-only behavior.
func priorMemoryProjection(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	digest, found, _ := priorMemoryProjectionDetailed(ctx, store, snap)
	return digest, found
}

func priorMemoryProjectionDetailed(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool, bool) {
	if snap.MemoryHash == "" {
		return ancestorMemoryProjectionDetailed(ctx, store, snap)
	}
	current, err := store.GetMemory(ctx, snap.MemoryHash)
	if err != nil {
		digest, found, _ := ancestorMemoryProjectionDetailed(ctx, store, snap)
		return digest, found, false
	}
	if len(current.Fragments) == 0 {
		return ancestorMemoryProjectionDetailed(ctx, store, snap)
	}
	projected, ok, complete := snapshotMemoryProjectionDetailed(ctx, store, snap)
	if !ok {
		return domain.MemoryDigest{}, false, complete
	}
	fragments := make([]domain.MemoryFragment, 0, len(projected.Fragments))
	for _, fragment := range projected.Fragments {
		if fragment.SourceSnapshot != snap.ID {
			fragments = append(fragments, fragment)
		}
	}
	if len(fragments) == 0 {
		return domain.MemoryDigest{}, false, complete
	}
	prior := domain.MemoryDigest{SnapshotID: snap.ID, Provider: projected.Provider, Fragments: fragments}
	return domain.MergeDigests(domain.MemoryDigest{}, prior), true, complete
}

func nearestDigestFrom(ctx context.Context, store outbound.SessionStore, start domain.ContentHash) (domain.MemoryDigest, bool) {
	return memoryProjectionFrom(ctx, store, start)
}

type memoryProjectionWalker struct {
	ctx            context.Context
	store          outbound.SessionStore
	seen           map[domain.ContentHash]bool
	supplementSeen map[domain.ContentHash]bool
	fingerprinter  *memoryProjectionFingerprinter
	projection     domain.MemoryDigest
	found          bool
	complete       bool
}

func memoryProjectionFrom(ctx context.Context, store outbound.SessionStore, starts ...domain.ContentHash) (domain.MemoryDigest, bool) {
	digest, found, _ := memoryProjectionFromDetailed(ctx, store, starts...)
	return digest, found
}

func memoryProjectionFromDetailed(ctx context.Context, store outbound.SessionStore, starts ...domain.ContentHash) (domain.MemoryDigest, bool, bool) {
	walker := memoryProjectionWalker{
		ctx: ctx, store: store,
		seen: map[domain.ContentHash]bool{}, supplementSeen: map[domain.ContentHash]bool{},
		fingerprinter: newMemoryProjectionFingerprinter(ctx, store), complete: true,
	}
	for _, start := range starts {
		walker.walk(start)
	}
	return walker.projection, walker.found, walker.complete
}

func (w *memoryProjectionWalker) walk(id domain.ContentHash) {
	if id == "" || w.seen[id] {
		return
	}
	w.seen[id] = true // Cycle guard: graft is mutable even though cycles are rejected at write time.
	snap, err := w.store.GetSnapshot(w.ctx, id)
	if err != nil {
		w.complete = false
		return // Partial local lineage: keep every available frontier.
	}

	digest, hasDigest := domain.MemoryDigest{}, false
	if snap.MemoryHash != "" {
		if loaded, loadErr := w.store.GetMemory(w.ctx, snap.MemoryHash); loadErr == nil {
			digest, hasDigest = loaded, true
		} else {
			w.complete = false
		}
	}
	if hasDigest && w.memoryDigestCoversLineage(digest, snap) {
		w.merge(digest)
		return
	}

	if hasDigest && len(digest.Fragments) == 0 {
		// A legacy cumulative digest already contains its natural lineage, but it
		// may have been produced by the old first-BFS-winner algorithm and silently
		// skipped a graft below that lineage. Scan natural ancestors only for such
		// hidden graft supplements, project this snapshot's current grafts, then
		// keep the nearest opaque digest once. This repairs old descendants without
		// concatenating every historical cumulative summary again.
		for _, parent := range snap.Parents {
			w.scanLegacySupplements(parent)
		}
		for _, parent := range snap.GraftParents {
			w.walk(parent)
		}
		w.mergeLegacyOpaque(digest)
		return
	}

	// No digest, or a modern fragment digest created against a stale/unknown
	// graft register: reconstruct all current parent frontiers first. Fragment
	// provenance then lets us append only this snapshot's own contribution, so
	// superseded grafts are not retained.
	for _, parent := range snap.ReachabilityParents() {
		w.walk(parent)
	}
	if hasDigest {
		if own, ok := w.retainedMemoryContribution(digest, snap); ok {
			w.merge(own)
		}
	}
}

// scanLegacySupplements follows the natural chain assumed to be embedded in a
// nearest opaque legacy digest and adds only graft branches that the old
// single-winner projection could have skipped. This scan cannot stop at a
// descendant digest's coverage: the outer opaque digest may predate a later
// memory/graft update on that ancestor.
func (w *memoryProjectionWalker) scanLegacySupplements(id domain.ContentHash) {
	if id == "" || w.seen[id] || w.supplementSeen[id] {
		return
	}
	w.supplementSeen[id] = true
	snap, err := w.store.GetSnapshot(w.ctx, id)
	if err != nil {
		w.complete = false
		return
	}
	for _, parent := range snap.Parents {
		w.scanLegacySupplements(parent)
	}
	for _, parent := range snap.GraftParents {
		w.walk(parent)
	}
}

func (w *memoryProjectionWalker) merge(digest domain.MemoryDigest) {
	if !w.found {
		w.projection = digest
		w.found = true
		return
	}
	w.projection = domain.MergeDigests(w.projection, digest)
}

// mergeLegacyOpaque avoids recreating the historical cumulative-summary
// explosion while repairing old grafts. Narrative replacement is allowed only
// when the later opaque digest byte-for-byte contains every projected fragment
// summary. Structured facts/tasks are still unioned explicitly, so narrative
// containment cannot discard them. No fuzzy or semantic match is inferred.
func (w *memoryProjectionWalker) mergeLegacyOpaque(digest domain.MemoryDigest) {
	if w.found && legacyDigestContainsProjectionNarrative(digest, w.projection) {
		digest.KeyFacts = mergeExactStrings(w.projection.KeyFacts, digest.KeyFacts)
		digest.OpenTasks = mergeExactStrings(w.projection.OpenTasks, digest.OpenTasks)
		w.projection = digest
		return
	}
	w.merge(digest)
}

func legacyDigestContainsProjectionNarrative(legacy, projection domain.MemoryDigest) bool {
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

func memoryGraftCoverage(
	ctx context.Context,
	store outbound.SessionStore,
	snap domain.Snapshot,
	fragments []domain.MemoryFragment,
	projectionComplete bool,
) *domain.MemoryGraftCoverage {
	fingerprinter := newMemoryProjectionFingerprinter(ctx, store)
	fingerprint, lineageComplete := fingerprinter.root(snap)
	state := memoryProjectionReadState{
		snap: snap, fingerprint: fingerprint, lineageComplete: lineageComplete,
		fingerprinter: fingerprinter,
	}
	return memoryGraftCoverageFromState(ctx, store, state, fragments, projectionComplete)
}

func memoryGraftCoverageFromState(
	ctx context.Context,
	store outbound.SessionStore,
	state memoryProjectionReadState,
	fragments []domain.MemoryFragment,
	projectionComplete bool,
) *domain.MemoryGraftCoverage {
	seenSources := map[domain.ContentHash]bool{}
	pinned := make([]domain.ContentHash, 0)
	for _, fragment := range fragments {
		source := fragment.SourceSnapshot
		if source == "" || source == state.snap.ID || seenSources[source] {
			continue
		}
		seenSources[source] = true
		// A source is reproducible only when it is in this root's current
		// reachability graph and its currently attached memory is readable.
		// Everything else is an intentional import or an available fallback from
		// a partial projection, and remains owned by this digest.
		reproducible := memorySourceReproducible(ctx, store, state.fingerprinter.reachable, source)
		if !reproducible {
			pinned = append(pinned, source)
		}
	}
	return &domain.MemoryGraftCoverage{
		ProjectionVersion:  domain.MemoryProjectionVersion,
		ProjectionComplete: projectionComplete && state.lineageComplete,
		LineageFingerprint: state.fingerprint,
		GraftSeq:           state.snap.GraftSeq,
		GraftParents:       append([]domain.ContentHash(nil), state.snap.GraftParents...),
		PinnedSources:      pinned,
	}
}

type memoryProjectionReadState struct {
	snap            domain.Snapshot
	fingerprint     domain.ContentHash
	ancestorState   domain.ContentHash
	lineageComplete bool
	fingerprinter   *memoryProjectionFingerprinter
}

func readMemoryProjectionState(ctx context.Context, store outbound.SessionStore, id domain.ContentHash) (memoryProjectionReadState, error) {
	snap, err := store.GetSnapshot(ctx, id)
	if err != nil {
		return memoryProjectionReadState{}, err
	}
	fingerprinter := newMemoryProjectionFingerprinter(ctx, store)
	fingerprint, rootComplete := fingerprinter.root(snap)
	fingerprinter.visiting[snap.ID] = true
	ancestorState, ancestorComplete := fingerprinter.snapshot(snap, true)
	delete(fingerprinter.visiting, snap.ID)
	return memoryProjectionReadState{
		snap: snap, fingerprint: fingerprint, ancestorState: ancestorState,
		lineageComplete: rootComplete && ancestorComplete,
		fingerprinter:   fingerprinter,
	}, nil
}

func sameMemoryProjectionState(left, right memoryProjectionReadState) bool {
	if left.snap.ID != right.snap.ID || left.snap.MemoryHash != right.snap.MemoryHash ||
		left.snap.GraftSeq != right.snap.GraftSeq || left.lineageComplete != right.lineageComplete ||
		left.fingerprint != right.fingerprint || left.ancestorState != right.ancestorState ||
		len(left.snap.GraftParents) != len(right.snap.GraftParents) {
		return false
	}
	for i := range left.snap.GraftParents {
		if left.snap.GraftParents[i] != right.snap.GraftParents[i] {
			return false
		}
	}
	return true
}

// childMemoryProjectionState derives a new single-parent seed's proof from the
// exact stable parent state used to build its memory. The parent edge uses the
// ancestor form (including the parent's MemoryHash); deriving it avoids a
// second live traversal that could stamp the seed with a concurrent state.
func childMemoryProjectionState(snap domain.Snapshot, parent memoryProjectionReadState) memoryProjectionReadState {
	fingerprinter := &memoryProjectionFingerprinter{reachable: map[domain.ContentHash]bool{}}
	for id := range parent.fingerprinter.reachable {
		fingerprinter.reachable[id] = true
	}
	fingerprinter.reachable[snap.ID] = true
	state := memoryProjectionReadState{snap: snap, fingerprinter: fingerprinter}
	if len(snap.Parents) != 1 || snap.Parents[0] != parent.snap.ID || len(snap.GraftParents) != 0 ||
		!parent.lineageComplete || parent.ancestorState == "" {
		return state
	}
	wire := memoryProjectionFingerprintState{
		SnapshotID:   snap.ID,
		GraftParents: []memoryProjectionFingerprintEdge{},
		Parents: []memoryProjectionFingerprintEdge{{
			SnapshotID: parent.snap.ID,
			State:      parent.ancestorState,
		}},
		GraftSeq: snap.GraftSeq,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return state
	}
	state.fingerprint = domain.HashContent(data)
	state.ancestorState = state.fingerprint // synthetic root has no MemoryHash yet
	state.lineageComplete = true
	return state
}

const memoryProjectionReadAttempts = 3

// stablePriorMemoryProjection prevents a projection from one graft/memory
// state from being stamped with proof of another state. Multiple terminals can
// update the same local .cxt store, so read the transitive state before and
// after projection and retry optimistically on movement.
func stablePriorMemoryProjection(
	ctx context.Context,
	store outbound.SessionStore,
	id domain.ContentHash,
) (memoryProjectionReadState, domain.MemoryDigest, bool, bool, error) {
	for attempt := 0; attempt < memoryProjectionReadAttempts; attempt++ {
		before, err := readMemoryProjectionState(ctx, store, id)
		if err != nil {
			return memoryProjectionReadState{}, domain.MemoryDigest{}, false, false, err
		}
		prior, found, projectionComplete := priorMemoryProjectionDetailed(ctx, store, before.snap)
		after, err := readMemoryProjectionState(ctx, store, id)
		if err != nil {
			return memoryProjectionReadState{}, domain.MemoryDigest{}, false, false, err
		}
		if sameMemoryProjectionState(before, after) {
			return after, prior, found, projectionComplete, nil
		}
	}
	return memoryProjectionReadState{}, domain.MemoryDigest{}, false, false,
		fmt.Errorf("%w: memory lineage changed during projection", domain.ErrSyncConflict)
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
	store     outbound.SessionStore
	memo      map[domain.ContentHash]memoryProjectionFingerprintResult
	visiting  map[domain.ContentHash]bool
	reachable map[domain.ContentHash]bool
}

func newMemoryProjectionFingerprinter(ctx context.Context, store outbound.SessionStore) *memoryProjectionFingerprinter {
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
	own := domain.MemoryDigest{SnapshotID: snap.ID, Provider: digest.Provider, Fragments: fragments}
	return domain.MergeDigests(domain.MemoryDigest{}, own), true
}

func memorySourceReproducible(
	ctx context.Context,
	store outbound.SessionStore,
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
	_, err = store.GetMemory(ctx, sourceSnapshot.MemoryHash)
	return err == nil
}

// nearestAncestorDigest is retained for explicit legacy single-nearest callers
// and compatibility tests. Project inheritance must use ancestorMemoryProjection.
func nearestAncestorDigest(ctx context.Context, store outbound.SessionStore, snap domain.Snapshot) (domain.MemoryDigest, bool) {
	seen := map[domain.ContentHash]bool{}
	queue := append([]domain.ContentHash{}, snap.ReachabilityParents()...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ps, err := store.GetSnapshot(ctx, id)
		if err != nil {
			continue
		}
		if ps.MemoryHash != "" {
			if d, derr := store.GetMemory(ctx, ps.MemoryHash); derr == nil {
				return d, true
			}
		}
		queue = append(queue, ps.ReachabilityParents()...)
	}
	return domain.MemoryDigest{}, false
}

// memoryCarryBudgetBytes bounds the summary carried forward by a merged digest
// (#33 — bounded carry, user-approved policy). MergeDigests concatenates
// generations oldest-first, so the forward working set grew without bound
// (measured 768KB in the dogfood repo). The carried copy keeps the newest
// tail; every prior generation's full digest object stays attached to its
// ancestor snapshot (content-addressed, never deleted), so complete history
// remains recoverable through the parent chain.
const memoryCarryBudgetBytes = 256 << 10
const memoryCarryListBudgetBytes = 64 << 10

// boundCarriedDigest caps only an inherited digest before it is merged into a
// fresh generation. The fresh digest itself is never truncated: otherwise a
// >256KiB native memory or newly generated summary would be lossy on its first
// storage and there would be no full ancestor object to recover it from.
func boundCarriedDigest(d domain.MemoryDigest) domain.MemoryDigest {
	// A carried digest becomes future provider context. Keep immutable archive
	// objects untouched, but do not copy legacy tool facts or unattested task
	// unions into another active generation.
	d = domain.PromptStructuredProjection(d)
	// Legacy memories can contain recursively materialized cxt seed summaries.
	// They remain immutable on their ancestor snapshots, but must not be copied
	// into the active projection again. Structured facts/tasks are retained and
	// the fresh snapshot contributes a clean extractive/provider summary.
	if hasNestedSeedSummary(d.Summary) {
		d.Summary = ""
	}
	if len(d.Summary) > memoryCarryBudgetBytes {
		d.Summary = truncateUTF8Tail(d.Summary, memoryCarryBudgetBytes)
	}
	d.KeyFacts = boundStringListTail(d.KeyFacts, memoryCarryListBudgetBytes)
	d.OpenTasks = boundStringListTail(d.OpenTasks, memoryCarryListBudgetBytes)
	if len(d.Fragments) > 0 {
		remainingSummary := memoryCarryBudgetBytes
		remainingFacts := memoryCarryListBudgetBytes
		remainingTasks := memoryCarryListBudgetBytes
		kept := make([]domain.MemoryFragment, 0, len(d.Fragments))
		for i := len(d.Fragments) - 1; i >= 0; i-- {
			fragment := d.Fragments[i]
			if hasNestedSeedSummary(fragment.Summary) {
				fragment.Summary = ""
			}
			fragment.Summary = truncateUTF8Tail(fragment.Summary, remainingSummary)
			fragment.KeyFacts = boundStringListTail(fragment.KeyFacts, remainingFacts)
			fragment.OpenTasks = boundStringListTail(fragment.OpenTasks, remainingTasks)
			if fragment.Summary == "" && len(fragment.KeyFacts) == 0 && len(fragment.OpenTasks) == 0 && !fragment.TasksAuthoritative {
				continue
			}
			remainingSummary -= len(fragment.Summary)
			remainingFacts -= stringListBytes(fragment.KeyFacts)
			remainingTasks -= stringListBytes(fragment.OpenTasks)
			kept = append(kept, fragment)
			if remainingSummary <= 0 && remainingFacts <= 0 && remainingTasks <= 0 {
				break
			}
		}
		for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
			kept[left], kept[right] = kept[right], kept[left]
		}
		d.Fragments = kept
	}
	return d
}

func hasNestedSeedSummary(summary string) bool {
	return strings.Contains(summary, seedSummaryPrefix) ||
		strings.Contains(summary, "[cxt seed] Branch-switch context:")
}

func stringListBytes(items []string) int {
	total := 0
	for _, item := range items {
		total += len(item)
	}
	return total
}

func boundStringListTail(items []string, maxBytes int) []string {
	if maxBytes <= 0 || len(items) == 0 {
		return nil
	}
	used := 0
	start := len(items)
	for start > 0 {
		n := len(items[start-1])
		if used+n > maxBytes {
			break
		}
		used += n
		start--
	}
	if start == 0 {
		return items
	}
	if start == len(items) {
		return []string{truncateUTF8Tail(items[len(items)-1], maxBytes)}
	}
	return append([]string(nil), items[start:]...)
}
