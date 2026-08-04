package app

import (
	"context"

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
//  4. MemorySource[Provider].ReadNative(Cwd) → (native, found). Native priority.
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
	if provider == "" {
		provider = domain.ProviderClaude
	}
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

	// Absorb native memory (e.g., CLAUDE.md/MEMORY.md) first — fallback to CIR distillation if not present.
	var nativePtr *domain.NativeMemory
	if src, ok := s.memSources[provider]; ok {
		if native, found, nerr := src.ReadNative(ctx, in.Cwd); nerr == nil && found {
			nativePtr = &native
		}
	}
	digest, err := s.distiller.Distill(ctx, doc.CIR, nativePtr)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	// Memory follows the same logic as raw — merges closest ancestor digest to inherit (natural grafting irrelevant, memory follows parent links).
	// Filter noisy prior KeyFacts so tool names and ingestion markers do not propagate forever across generations. Keep only sentence-form facts, using the same rules as the seed filter.
	if prior, ok := nearestAncestorDigest(ctx, s.store, snap); ok {
		prior.KeyFacts = seedWorthyFacts(prior.KeyFacts)
		digest = domain.MergeDigests(prior, digest)
	}
	digest = boundCarriedDigest(digest)
	digest.SnapshotID = snap.ID

	memHash, err := s.store.PutMemory(ctx, digest)
	if err != nil {
		return inbound.MemorizeOutput{}, err
	}
	// Attach to current branch HEAD snapshot (derivative pointer — ID/body immutable).
	snap.MemoryHash = memHash
	if err := s.store.PutSnapshot(ctx, snap); err != nil {
		return inbound.MemorizeOutput{}, err
	}
	return inbound.MemorizeOutput{SnapshotID: snap.ID, MemoryHash: memHash, Attached: true}, nil
}

// Ensure MemorizeService implements inbound.Memorize.
var _ inbound.Memorize = (*MemorizeService)(nil)

// nearestAncestorDigest finds the MemoryDigest of the nearest ancestor of snap by traversing the reachability parent chain using BFS. If no ancestor is found, ok=false. It walks the raw lineage (parent links) directly, so it doesn't distinguish between natural inheritance, append overlay grafts, or any other logic. The ancestor itself was merged with its parent at the distillation point, so finding the closest one is sufficient (minimal).
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
			continue // Skip local ancestors (partial lineage) — within the available range, the best option
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

// boundCarriedDigest caps the summary of a digest that will be carried forward
// (stored on a new snapshot or injected as a provider memory file).
func boundCarriedDigest(d domain.MemoryDigest) domain.MemoryDigest {
	if len(d.Summary) > memoryCarryBudgetBytes {
		d.Summary = truncateUTF8Tail(d.Summary, memoryCarryBudgetBytes)
	}
	return d
}
