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
	memSources    map[domain.ProviderKind]outbound.MemorySource
}

// NewBranchSeedService creates BranchSeedService and injects dependencies.
func NewBranchSeedService(
	gitCtx outbound.GitContext,
	store outbound.SessionStore,
	distiller outbound.MemoryDistiller,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	materializers map[domain.ProviderKind]outbound.SessionMaterializer,
	memSources map[domain.ProviderKind]outbound.MemorySource,
) *BranchSeedService {
	return &BranchSeedService{
		gitCtx: gitCtx, store: store, distiller: distiller,
		codecs: codecs, materializers: materializers, memSources: memSources,
	}
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
	targetCwd := in.Cwd
	if targetCwd == "" {
		targetCwd = repo.LocalPath
	}
	targetCwd = materializationCwd(targetCwd)

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
			if d, ok := snapshotMemoryProjection(ctx, s.store, mainSnap); ok {
				mainMem = &d
			}
		}
		if mainMem == nil && mainDocAvailable {
			if d, derr := s.distiller.Distill(ctx, mainDoc.CIR.EffectiveContext(), nil); derr == nil {
				d.SnapshotID = mainSnap.ID
				d = domain.MergeDigests(domain.MemoryDigest{}, d)
				if prior, ok := priorMemoryProjection(ctx, s.store, mainSnap); ok {
					prior = boundCarriedDigest(prior)
					d = domain.MergeDigests(prior, d)
				}
				mainMem = &d
			}
		}
	}

	// Layer 2: departure branch on-the-fly distillation (+ ancestor inheritance) — always fresh without relying on stored digest.
	branchContext := fromDoc.CIR.EffectiveContext()
	freshBranchMem, err := s.distiller.Distill(ctx, branchContext, nil)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	freshBranchMem.SnapshotID = fromSnap.ID
	// The source conversation is replayed verbatim below. Keep its compact/native
	// baseline in the prompt, but not the canonical current-conversation delta.
	// The full fresh digest is still attached to the seed snapshot.
	promptFreshBranchMem := domain.WithoutConversationDeltaFromSource(freshBranchMem, fromSnap.ID)
	branchMem := freshBranchMem
	promptBranchMem := promptFreshBranchMem
	branchState, prior, hasBranchPrior, branchProjectionComplete, err := stablePriorMemoryProjection(ctx, s.store, fromSnap.ID)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	fromSnap = branchState.snap
	if hasBranchPrior {
		prior = boundCarriedDigest(prior)
		branchMem = domain.MergeDigests(prior, branchMem)
		promptBranchMem = domain.MergeDigests(prior, promptBranchMem)
	}
	// Preserve the departure snapshot as provenance even when it had no prior
	// digest. The seed changes SnapshotID below, but inherited fragments must
	// keep identifying their actual source for future stale-lineage repair.
	branchMem = domain.MergeDigests(domain.MemoryDigest{}, branchMem)
	promptBranchMem = domain.MergeDigests(domain.MemoryDigest{}, promptBranchMem)
	mainPromptMem := mainMem
	if mainPromptMem != nil && mainBranch == in.FromBranch {
		promptCopy := domain.WithoutConversationDeltaFromSource(*mainPromptMem, fromSnap.ID)
		mainPromptMem = &promptCopy
	}
	// A branch seed is a new provider session. Remove only a baseline that the
	// target provider guarantees it will independently load for this working
	// tree. The attached seedMemory below keeps the complete portable digest.
	if native, ok := readTargetNativeMemory(
		ctx, s.memSources, provider, targetCwd, fromDoc.CIR.Envelope.SessionOriginID,
	); ok {
		if mainPromptMem != nil {
			projected := projectAutoLoadedNative(*mainPromptMem, native)
			mainPromptMem = &projected
		}
		promptBranchMem = projectAutoLoadedNative(promptBranchMem, native)
	}

	// Seed CIR synthesis: [Layer1⊕Layer2 summary message] + [Layer3 bounded
	// full session]. Any semantic events cut from Layer3 become a bounded bridge
	// digest before the final prompt is accepted. Without that bridge, a source
	// with an older provider compaction summary silently loses post-compaction
	// work between the summary and the retained tail (#84).
	now := time.Now().UTC().Format(time.RFC3339)
	seedSession := providerfs.NewSessionID()
	events, seedBranchMemory, err := s.buildBranchSeedEvents(
		ctx, provider, in.FromBranch, in.NewBranch, now, fromSnap.ID,
		mainPromptMem, promptBranchMem, branchMem, seedConversationContext(branchContext),
	)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	cir := domain.CIRDocument{Events: events}
	cir.Envelope = fromDoc.CIR.Envelope // Inherit model, cwd, etc.
	cir.Envelope.CIRVersion = domain.CIRVersionForEvents(events)
	if targetCwd != "" {
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
	// The materialized prompt is intentionally bounded, but the branch must not
	// lose the inherited digest. Attach the merged main + departure-lineage
	// memory to the seed snapshot so the next memorize/seed operation inherits
	// it rather than re-distilling the truncated summary event. The carried
	// copy is bounded (#33 — newest tail); older generations stay reachable
	// through the parent chain's memory objects.
	seedMemory := seedBranchMemory
	if mainMem != nil {
		seedMemory = domain.MergeDigests(boundCarriedDigest(*mainMem), seedBranchMemory)
	}
	seedMemory.SnapshotID = docHash
	// A seed is a new snapshot, so its first attachment is a causal root even
	// when its projected content was imported from another snapshot's memory.
	seedMemory.PreviousMemoryHash = ""
	seedState := childMemoryProjectionState(snap, branchState)
	seedMemory.GraftCoverage = memoryGraftCoverageFromState(ctx, s.store, seedState, seedMemory.Fragments, branchProjectionComplete)
	if seedMemory.Provider == "" {
		seedMemory.Provider = provider
	}
	memoryHash, err := s.store.PutMemory(ctx, seedMemory)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	snap.MemoryHash = memoryHash
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

// buildBranchSeedEvents partitions the source conversation into a bridge
// distilled from the exact omitted slice and a verbatim recent tail. Rendering the bridge can enlarge
// the summary event, so the loop only moves the cut forward until the final
// encoded event list fits; it never re-expands a tail and therefore converges
// after at most one pass per source event.
func (s *BranchSeedService) buildBranchSeedEvents(
	ctx context.Context,
	provider domain.ProviderKind,
	from, to, now string,
	sourceSnapshot domain.ContentHash,
	mainMemory *domain.MemoryDigest,
	promptBranchMemory domain.MemoryDigest,
	storedBranchMemory domain.MemoryDigest,
	conversation domain.CIRDocument,
) ([]domain.Event, domain.MemoryDigest, error) {
	totalBudget, digestBudget := seedBudgets(provider)
	promptMemory := promptBranchMemory
	maxConversationBudget := totalBudget
	for attempt := 0; attempt <= len(conversation.Events)+1; attempt++ {
		seedText := renderSeedText(from, to, mainMemory, promptMemory, digestBudget)
		summaryEvent := domain.Event{
			Kind: domain.EventMessage, Role: "user", Ts: now, Seq: 0,
			Blocks: []domain.ContentBlock{{Type: "text", Text: seedText}},
		}
		conversationBudget := totalBudget - eventsJSONBytes([]domain.Event{summaryEvent})
		if conversationBudget < 0 {
			conversationBudget = 0
		}
		if conversationBudget > maxConversationBudget {
			conversationBudget = maxConversationBudget
		} else {
			maxConversationBudget = conversationBudget
		}
		trimmed, omitted := trimEventsForSeed(conversation, conversationBudget)

		mergedPromptMemory := promptBranchMemory
		mergedStoredMemory := storedBranchMemory
		if len(omitted) > 0 {
			omittedCIR := conversation
			omittedCIR.Events = append([]domain.Event(nil), omitted...)
			bridge, err := s.distiller.Distill(ctx, omittedCIR, nil)
			if err != nil {
				return nil, domain.MemoryDigest{}, err
			}
			bridge.SnapshotID = sourceSnapshot
			mergedPromptMemory = domain.MergeDigests(promptBranchMemory, bridge)
			mergedStoredMemory = domain.MergeDigests(storedBranchMemory, bridge)
		}

		// The bridge itself changes the summary size. Re-render before checking
		// the actual wire-shaped event budget; if it grew, the next pass trims
		// only more verbatim events and includes them in a new bridge.
		seedText = renderSeedText(from, to, mainMemory, mergedPromptMemory, digestBudget)
		summaryEvent.Blocks[0].Text = seedText
		events := []domain.Event{summaryEvent}
		for i, event := range trimmed.Events {
			event.Seq = i + 1
			events = append(events, event)
		}
		if eventsJSONBytes(events) <= totalBudget {
			return events, mergedStoredMemory, nil
		}
		promptMemory = mergedPromptMemory
	}
	return nil, domain.MemoryDigest{}, fmt.Errorf("branch seed context did not converge within %d bytes", totalBudget)
}

// seedConversationContext keeps only the human/agent conversation needed by a
// new branch. The summary layer above already carries project memory, so
// replaying prior synthetic cxt seeds, provider compact summaries, runtime
// developer/system instructions, harness environment blocks, or an encrypted
// compaction state would duplicate or invalidate context in the new session.
// The archival source document is never mutated.
func seedConversationContext(cir domain.CIRDocument) domain.CIRDocument {
	out := cir.EffectiveContext()
	events := make([]domain.Event, 0, len(out.Events))
	for _, ev := range out.Events {
		if ev.Kind == domain.EventCompaction {
			continue
		}
		if ev.Kind == domain.EventMessage {
			if ev.CompactSummary || ev.Role == "system" || ev.Role == "developer" || isSyntheticReplayMessage(ev) {
				continue
			}
			if ev.AgentMessage {
				// A new branch is a new provider session. Keep the visible agent
				// result as assistant context, but never transplant provider-local
				// routing identities or encrypted subagent state across sessions.
				ev.AgentMessage = false
				ev.AgentAuthor = ""
				ev.AgentRecipient = ""
				ev.Locked = nil
				if !hasVisibleMessageText(ev) {
					continue
				}
			}
		}
		events = append(events, ev)
	}
	out.Events = events
	out.Envelope.CIRVersion = domain.CIRVersionForEvents(events)
	return out
}

func hasVisibleMessageText(ev domain.Event) bool {
	for _, block := range ev.Blocks {
		if strings.TrimSpace(block.Text) != "" {
			return true
		}
	}
	return false
}

func isSyntheticReplayMessage(ev domain.Event) bool {
	if ev.Kind != domain.EventMessage || ev.Role != "user" {
		return false
	}
	for _, block := range ev.Blocks {
		text := strings.TrimSpace(block.Text)
		if text == "" {
			continue
		}
		return strings.HasPrefix(text, "[cxt seed] Branch-switch context:") ||
			strings.HasPrefix(text, seedSummaryPrefix) ||
			strings.HasPrefix(text, "<environment_context>")
	}
	return false
}

// renderSeedText renders the seed's summary layer (Layer1⊕Layer2) as labeled markdown.
// Removes overlapping key facts/tasks from Layer2 (maintains layer distinction + removes duplicates).
//
// Sections are budgeted individually (resume-path parity with renderSeedDigest):
// MergeDigests puts the oldest summary generations first, so a single whole-text
// prefix cut would keep only stale summary head and silently drop key facts,
// open tasks, the entire Layer2 digest, and the trailer once the main summary
// outgrows the digest budget. Bullets are reserved first and summaries keep
// their newest tail; exact byte accounting guarantees the result fits maxBytes.
func renderSeedText(from, to string, mainMem *domain.MemoryDigest, branchMem domain.MemoryDigest, maxBytes int) string {
	header := fmt.Sprintf("[cxt seed] Branch-switch context: %s → %s\n", from, to) +
		"This session is the seed of a new branch — continue from the summaries and the verbatim recent context below.\n"
	trailer := "\n## Recent context (verbatim)\nThe events below are the actual conversation right before the switch.\n"
	mainHeader := "\n## Project understanding (main)\n"
	branchHeader := "\n## Work summary of this lineage (" + from + ")\n"

	budget := maxBytes - len(header) - len(trailer) - len(branchHeader)
	if mainMem != nil {
		budget -= len(mainHeader)
	}
	if budget <= 0 {
		return truncateUTF8Prefix(header, maxBytes)
	}

	// Bullets first — the distilled high-value layer must survive an oversized summary.
	seen := map[string]bool{}
	var mainFacts, mainTasks string
	if mainMem != nil {
		mainFacts = seedBulletLines(seedWorthyFacts(mainMem.KeyFacts), "- ", seen, budget/6)
		mainTasks = seedBulletLines(mainMem.OpenTasks, "- ☐ ", seen, budget/8)
	}
	branchFacts := seedBulletLines(seedWorthyFacts(branchMem.KeyFacts), "- ", seen, budget/8)
	branchTasks := seedBulletLines(branchMem.OpenTasks, "- ☐ ", seen, budget/8)
	remaining := budget - len(mainFacts) - len(mainTasks) - len(branchFacts) - len(branchTasks)

	// Summaries share what is left; each keeps its newest tail. The branch
	// summary is capped so a large lineage digest cannot starve the main layer
	// (and vice versa — the main summary takes only the final remainder).
	mainSummary, branchSummary := "", ""
	if s := strings.TrimSpace(branchMem.Summary); s != "" && remaining > 1 {
		limit := remaining
		if mainMem != nil && strings.TrimSpace(mainMem.Summary) != "" && limit > budget/4 {
			limit = budget / 4
		}
		branchSummary = truncateUTF8Tail(s, limit-1) + "\n"
		seen[s] = true
		remaining -= len(branchSummary)
	}
	if mainMem != nil {
		if s := strings.TrimSpace(mainMem.Summary); s != "" && !seen[s] && remaining > 1 {
			mainSummary = truncateUTF8Tail(s, remaining-1) + "\n"
		}
	}

	var b strings.Builder
	b.WriteString(header)
	if mainMem != nil {
		b.WriteString(mainHeader)
		b.WriteString(mainSummary)
		b.WriteString(mainFacts)
		b.WriteString(mainTasks)
	}
	b.WriteString(branchHeader)
	b.WriteString(branchSummary)
	b.WriteString(branchFacts)
	b.WriteString(branchTasks)
	b.WriteString(trailer)
	return b.String()
}

// seedBulletLines renders "- item" lines within maxBytes, skipping items already
// emitted by an earlier layer (seen dedup shared across Layer1/Layer2).
func seedBulletLines(items []string, prefix string, seen map[string]bool, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		line := prefix + item + "\n"
		if b.Len()+len(line) > maxBytes {
			continue // keep scanning — a shorter later item may still fit
		}
		seen[item] = true
		b.WriteString(line)
	}
	return b.String()
}
