package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// BranchSeedService implements the SeedBranch use-case.
//
// Context switch logic (user agreement): New branches are born with only the necessary layers —
//
//	[Project Understanding] main head's MemoryDigest        (long-term memory)
//	[Changelog Summary] departure branch head's on-the-fly distillation      (mid-term summary — including ancestor inheritance, items overlapping with main layer removed)
//	[Previous Context] departure head's bounded full session   (conversation at the switch point)
//
// The seed is committed as the first snapshot of the new branch (branch birth — cut meaning), and serialized to a session file (ledger record — capture excluded for resumption).
type BranchSeedService struct {
	gitCtx        outbound.GitContext
	store         outbound.SessionStore
	distiller     outbound.MemoryDistiller
	codecs        map[domain.ProviderKind]outbound.ProviderCodec
	materializers map[domain.ProviderKind]outbound.SessionMaterializer
}

// NewBranchSeedService creates BranchSeedService and injects dependencies.
func NewBranchSeedService(
	gitCtx outbound.GitContext,
	store outbound.SessionStore,
	distiller outbound.MemoryDistiller,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	materializers map[domain.ProviderKind]outbound.SessionMaterializer,
) *BranchSeedService {
	return &BranchSeedService{gitCtx: gitCtx, store: store, distiller: distiller, codecs: codecs, materializers: materializers}
}

var _ inbound.SeedBranch = (*BranchSeedService)(nil)

// Seed creates a new branch seed from FromBranch materials and commits/serializes it.
func (s *BranchSeedService) Seed(ctx context.Context, in inbound.SeedInput) (inbound.SeedOutput, error) {
	provider := in.Provider
	if provider == "" {
		provider = domain.ProviderClaude
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	fromRef, err := s.store.GetRef(ctx, repo.ID, domain.RefBranch, in.FromBranch)
	if err != nil || fromRef.Target == "" {
		return inbound.SeedOutput{}, domain.ErrNotFound // no context for departure branch
	}
	fromSnap, err := s.store.GetSnapshot(ctx, fromRef.Target)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	fromDoc, err := s.store.GetDoc(ctx, fromSnap.DocHash)
	if err != nil {
		return inbound.SeedOutput{}, err
	}

	// Layer 1: main head memory (explicit project understanding), including
	// when the departure branch itself is main. The post-checkout hook
	// checkpoints and memorizes main immediately before this service runs; the
	// old equality guard discarded exactly that freshly attached digest.
	mainBranch := repo.DefaultBranch
	if mainBranch == "" {
		mainBranch = "main"
	}
	var mainMem *domain.MemoryDigest
	var mainSnap domain.Snapshot
	var mainDoc domain.SessionDoc
	mainDocAvailable := false
	if mainBranch == in.FromBranch {
		mainSnap, mainDoc = fromSnap, fromDoc
		mainDocAvailable = true
	} else if mref, merr := s.store.GetRef(ctx, repo.ID, domain.RefBranch, mainBranch); merr == nil && mref.Target != "" {
		if msnap, serr := s.store.GetSnapshot(ctx, mref.Target); serr == nil {
			mainSnap = msnap
			if mdoc, derr := s.store.GetDoc(ctx, msnap.DocHash); derr == nil {
				mainDoc = mdoc
				mainDocAvailable = true
			}
		}
	}
	if mainSnap.ID != "" {
		if mainSnap.MemoryHash != "" {
			if d, derr := s.store.GetMemory(ctx, mainSnap.MemoryHash); derr == nil {
				mainMem = &d
			}
		}
		if mainMem == nil && mainDocAvailable {
			if d, derr := s.distiller.Distill(ctx, mainDoc.CIR, nil); derr == nil {
				if prior, ok := nearestAncestorDigest(ctx, s.store, mainSnap); ok {
					d = domain.MergeDigests(prior, d)
				}
				mainMem = &d
			}
		}
	}

	// Layer 2: departure branch on-the-fly distillation (+ ancestor inheritance) — always fresh without relying on stored digest.
	branchMem, err := s.distiller.Distill(ctx, fromDoc.CIR, nil)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	if prior, ok := nearestAncestorDigest(ctx, s.store, fromSnap); ok {
		branchMem = domain.MergeDigests(prior, branchMem)
	}

	// Seed CIR synthesis: [Layer1⊕Layer2 summary message] + [Layer3 bounded
	// full session]. The summary has a fixed maximum, and the exact encoded
	// summary size is subtracted from the shared seed budget before selecting
	// the recent user-boundary-aligned conversation tail.
	now := time.Now().UTC().Format(time.RFC3339)
	seedSession := providerfs.NewSessionID()
	seedText := renderSeedText(in.FromBranch, in.NewBranch, mainMem, branchMem)
	seedText = truncateUTF8Prefix(seedText, seedDigestBudgetBytes)
	summaryEvent := domain.Event{
		Kind: domain.EventMessage, Role: "user", Ts: now, Seq: 0,
		Blocks: []domain.ContentBlock{{Type: "text", Text: seedText}},
	}
	conversationBudget := seedBudgetBytes - eventsJSONBytes([]domain.Event{summaryEvent})
	if conversationBudget < 0 {
		conversationBudget = 0
	}
	trimmedConversation, _ := trimEventsForSeed(fromDoc.CIR, conversationBudget)
	events := []domain.Event{summaryEvent}
	for i, ev := range trimmedConversation.Events {
		ev.Seq = i + 1
		events = append(events, ev)
	}
	cir := domain.CIRDocument{Events: events}
	cir.Envelope = fromDoc.CIR.Envelope // Inherit model, cwd, etc.
	targetCwd := in.Cwd
	if targetCwd == "" {
		targetCwd = repo.LocalPath
	}
	if targetCwd = materializationCwd(targetCwd); targetCwd != "" {
		cir.Envelope.Cwd = targetCwd
	}
	cir.Envelope.GitBranch = in.NewBranch
	cir.Envelope.SessionOriginID = seedSession
	cir.Envelope.CapturedAt = now
	cir.Envelope.Fidelity = domain.FidelityReconstructed
	cir.Envelope.ContextTokens = 0 // New seed — statistical reset
	cir.Envelope.OutputTokens = 0

	docHash, err := s.store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	snap := domain.Snapshot{
		ID: docHash, RepoID: repo.ID, Branch: in.NewBranch,
		Parents: []domain.ContentHash{fromRef.Target}, DocHash: docHash,
		Provider: provider, Fidelity: domain.FidelityReconstructed,
		Message:   fmt.Sprintf("seed: %s → %s", in.FromBranch, in.NewBranch),
		Author:    in.Author,
		CreatedAt: time.Now().UTC(),
		SessionID: seedSession,
		Models:    fromDoc.CIR.Envelope.OrderedModels(),
	}
	if err := s.store.PutSnapshot(ctx, snap); err != nil {
		return inbound.SeedOutput{}, err
	}
	if err := s.store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: in.NewBranch, RepoID: repo.ID, Target: docHash}); err != nil {
		return inbound.SeedOutput{}, err
	}
	_ = s.store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: in.NewBranch})

	out := inbound.SeedOutput{SnapshotID: docHash, SessionID: seedSession}
	// Materialization (best-effort): Seed snapshot is committed even on failure — can be restored with cxt checkout.
	if cdc, ok := s.codecs[provider]; ok {
		if mat, ok2 := s.materializers[provider]; ok2 {
			if raw, encErr := cdc.Encode(ctx, cir, provider); encErr == nil {
				if path, resume, mErr := mat.Materialize(ctx, raw, in.Cwd); mErr == nil {
					_ = providerfs.RecordMaterialized(in.Cwd, path)
					out.WrittenPath, out.ResumeCmd = path, resume
					// Materializers rewrite provider-native session IDs to avoid
					// colliding with the source session. The restart target is the
					// rewritten ID, not the synthetic CIR origin ID.
					if fields := strings.Fields(resume); len(fields) > 0 && providerfs.ValidSessionID(fields[len(fields)-1]) {
						out.SessionID = fields[len(fields)-1]
					}
				}
			}
		}
	}
	return out, nil
}

// renderSeedText renders the seed's summary layer (Layer1⊕Layer2) as labeled markdown.
// Removes overlapping key facts/tasks from Layer2 (maintains layer distinction + removes duplicates).
func renderSeedText(from, to string, mainMem *domain.MemoryDigest, branchMem domain.MemoryDigest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[cxt seed] Branch-switch context: %s → %s\n", from, to)
	b.WriteString("This session is the seed of a new branch — continue from the summaries and the verbatim recent context below.\n")
	seen := map[string]bool{}
	if mainMem != nil {
		b.WriteString("\n## Project understanding (main)\n")
		writeDigest(&b, *mainMem, seen)
	}
	b.WriteString("\n## Work summary of this lineage (" + from + ")\n")
	writeDigest(&b, branchMem, seen)
	b.WriteString("\n## Recent context (verbatim)\nThe events below are the actual conversation right before the switch.\n")
	return b.String()
}

func writeDigest(b *strings.Builder, d domain.MemoryDigest, seen map[string]bool) {
	if d.Summary != "" && !seen[d.Summary] {
		seen[d.Summary] = true
		b.WriteString(d.Summary + "\n")
	}
	for _, f := range d.KeyFacts {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		b.WriteString("- " + f + "\n")
	}
	for _, t := range d.OpenTasks {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		b.WriteString("- ☐ " + t + "\n")
	}
}
