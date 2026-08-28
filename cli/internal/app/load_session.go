package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// LoadSessionService implements the LoadSession inbound port as a use-case service.
//
// Dependency outbound ports: SessionStore, ProviderCodec (registry), SessionMaterializer (registry),
// MemorySource (registry), MemoryDistiller, MemorySink (registry).
//
// Load sequence (compatibility rules / backend architecture):
//  1. Resolve in.Ref → Determine snapshot ID
//     - "HEAD" → GetRef(RefHEAD).Symbolic → Branch HEAD
//     - Branch name → GetRef(RefBranch, name).Target
//     - Tag name → GetRef(RefTag, name).Target
//     - sha256:* → Direct use
//  2. SessionStore.GetSnapshot(snapID) + GetDoc(snap.DocHash)
//  3. Branch on Mode (compatibility rules):
//     full (or reconstructed):
//     a. ProviderCodec[TargetProvider].Encode(cir, TargetProvider)
//     b. SessionMaterializer[TargetProvider].Materialize(cir, Cwd) → (sessionPath, resumeCmd)
//     c. Prepend bounded portable memory not already represented by the replay.
//     d. LoadOutput{WrittenPath: sessionPath, ResumeCmd, Fidelity: full/reconstructed}
//     e. **Fallback to memory mode automatically on Materialize failure** (Fidelity downgrade → memory).
//     memory:
//     a. For same-provider or legacy replay, MemorySource[TargetProvider].ReadNative(Cwd, SessionOriginID) → (native, found)
//     b. MemoryDistiller.Distill(cir, native) → MemoryDigest (native==nil fallback to CIR distillation)
//     c. digest.SnapshotID = snap.ID (app layer injection)
//     d. MemorySink[TargetProvider].Inject(digest, Cwd) → writtenPath
//     e. Achieved fidelity = memory, ResumeCmd empty
//  4. Return LoadOutput{WrittenPath, ResumeCmd, Fidelity: achieved}
//
// CIR is the single source of truth (compatibility rules).
type LoadSessionService struct {
	store         outbound.SessionStore
	codecs        map[domain.ProviderKind]outbound.ProviderCodec
	materializers map[domain.ProviderKind]outbound.SessionMaterializer
	memSources    map[domain.ProviderKind]outbound.MemorySource
	distiller     outbound.MemoryDistiller
	memSinks      map[domain.ProviderKind]outbound.MemorySink
}

// NewLoadSessionService creates and injects dependencies into LoadSessionService.
func NewLoadSessionService(
	store outbound.SessionStore,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	materializers map[domain.ProviderKind]outbound.SessionMaterializer,
	memSources map[domain.ProviderKind]outbound.MemorySource,
	distiller outbound.MemoryDistiller,
	memSinks map[domain.ProviderKind]outbound.MemorySink,
) *LoadSessionService {
	return &LoadSessionService{
		store:         store,
		codecs:        codecs,
		materializers: materializers,
		memSources:    memSources,
		distiller:     distiller,
		memSinks:      memSinks,
	}
}

// Load restores a snapshot to a target provider session file.
// Mode=full/reconstructed performs full-context restoration (fallback to memory mode on failure), Mode=memory injects memory-form summary.
func (s *LoadSessionService) Load(ctx context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
	snapID, err := resolveRef(ctx, s.store, in.RepoID, in.Ref)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	if in.PreferPendingTail {
		snapID = s.pendingTailOf(ctx, in.RepoID, in.Ref, snapID, in.PreferredSessionID)
	}
	snap, err := s.store.GetSnapshot(ctx, snapID)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	doc, err := s.store.GetDoc(ctx, snap.DocHash)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	cir := doc.CIR

	target := in.TargetProvider
	if target == "" {
		target = cir.Envelope.SourceProvider
	}
	mode := in.Mode
	if mode == "" {
		mode = domain.FidelityFull
	}

	if mode == domain.FidelityMemory {
		return s.loadMemory(ctx, cir, snap, target, in.Cwd)
	}

	// full / reconstructed: codec.Encode → materializer.Materialize
	// Seed budget: Session doc can exceed target model context (empirically: weekly session summary → codex "ran out of room" like empty session).
	// Recent semantic events remain within budget — truncation point is a user
	// prompt boundary. Opaque provider state is never sliced and does not consume
	// that semantic allowance because no safe textual projection exists for it.
	// A same-provider replay may therefore exceed the target by the opaque bytes
	// required to resume the provider's compacted state.
	// Replay the provider-visible context, not the cumulative archival stream.
	// Legacy docs without an explicit compaction boundary return unchanged.
	replacementCount, hasCompleteReplacement := cir.LatestCompactionReplacementCount()
	preserveCodexReplacement := target == domain.ProviderCodex &&
		cir.Envelope.SourceProvider == domain.ProviderCodex &&
		hasCompleteReplacement
	seedCIR := cir.EffectiveContext()
	var omitted []domain.Event
	keptReplacement := preserveCodexReplacement
	totalBudget, digestBudget := seedBudgets(target)
	portableSeed, hasPortableSeed := s.portableReplaySeed(
		ctx, seedCIR, snap, target, in.Cwd, digestBudget,
	)
	projectedSeedCIR := seedCIR
	if hasPortableSeed {
		insertAfter := 0
		if preserveCodexReplacement {
			insertAfter = replacementCount
		}
		projectedSeedCIR.Events = replaceSyntheticSeedSummary(seedCIR.Events, portableSeed, insertAfter)
	}
	requiresTrim := eventsJSONBytes(seedCIR.Events) > totalBudget ||
		(hasPortableSeed && eventsJSONBytes(projectedSeedCIR.Events) > totalBudget)
	if requiresTrim {
		conversationBudget := totalBudget - digestBudget
		if preserveCodexReplacement {
			seedCIR, omitted, keptReplacement = trimEventsForSeedKeepingPrefix(seedCIR, replacementCount, conversationBudget)
		} else {
			seedCIR, omitted = trimEventsForSeed(seedCIR, conversationBudget)
		}
	}
	dropped := len(omitted)
	if dropped > 0 {
		// Omit memory digest of skipped segment at seed start with "CompactSummary" event — context compression equivalent (summary + recent raw). Viewer collapses, distiller 1st priority last-wins (memory ↔ context cycle accumulation block).
		// Distillation is mandatory: materializing a resumable tail without a
		// representation of the exact omitted span would silently lose context.
		omittedCIR := cir
		omittedCIR.Events = omitted
		var digestErr error
		if keptReplacement {
			seedCIR, digestErr = s.insertTrimDigestAfterPrefix(ctx, omittedCIR, seedCIR, replacementCount, snap, target, in.Cwd)
		} else {
			seedCIR, digestErr = s.prependTrimDigest(ctx, omittedCIR, seedCIR, snap, target, in.Cwd)
		}
		if digestErr != nil {
			return inbound.LoadOutput{}, fmt.Errorf("distill omitted context: %w", digestErr)
		}
	} else if hasPortableSeed {
		seedCIR = projectedSeedCIR
	}
	// A restored session belongs to the working tree it is being restored into,
	// not the machine/path where the source snapshot was captured. Codex active
	// session discovery reads payload.cwd, so retaining the source path makes a
	// relocated clone impossible to capture after resume.
	if targetCwd := materializationCwd(in.Cwd); targetCwd != "" {
		seedCIR.Envelope.Cwd = targetCwd
	}
	if preserveCodexReplacement {
		seedCIR = asCodexCompactedReplay(seedCIR)
	}
	cdc, okCodec := s.codecs[target]
	mat, okMat := s.materializers[target]
	if okCodec && okMat {
		if raw, encErr := cdc.Encode(ctx, seedCIR, target); encErr == nil {
			if path, resume, mErr := mat.Materialize(ctx, raw, in.Cwd); mErr == nil {
				// Ledger record: Recovery candidate is excluded from capture until actual resume (live session hijacking prevention — providerfs/ledger.go).
				_ = providerfs.RecordMaterialized(in.Cwd, path)
				fid := domain.FidelityReconstructed
				if target == cir.Envelope.SourceProvider {
					fid = domain.FidelityFull
				}
				return inbound.LoadOutput{WrittenPath: path, ResumeCmd: resume, Fidelity: fid, TrimmedEvents: dropped}, nil
			}
		}
	}
	// Fallback (compatibility rules): full restoration failure → memory mode downgrade.
	return s.loadMemory(ctx, cir, snap, target, in.Cwd)
}

// asCodexCompactedReplay restores Codex's native operation shape. Codex does
// not write encrypted compaction state as a top-level response_item; the whole
// effective history lives in compacted.replacement_history. The caller first
// projects, trims, and summarizes the semantic event stream, then this wrapper
// makes that bounded stream the authoritative replacement in the new rollout.
func asCodexCompactedReplay(cir domain.CIRDocument) domain.CIRDocument {
	replacement := append([]domain.Event{}, cir.Events...)
	ts := cir.Envelope.CapturedAt
	if len(replacement) > 0 && replacement[0].Ts != "" {
		ts = replacement[0].Ts
	}
	cir.Events = []domain.Event{{
		Kind:                domain.EventCompaction,
		Ts:                  ts,
		Seq:                 0,
		Replacement:         replacement,
		ReplacementComplete: true,
	}}
	cir.Envelope.CIRVersion = domain.CIRVersionV2
	return cir
}

func materializationCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	return abs
}

// pendingTailOf returns the latest pending (uncommitted hook capture) snapshot connecting to head.
// Acceptance conditions: (1) pending.Branch == loaded branch — branching off different branch's uncommitted conversation into same head not allowed (cross-branch leakage). (2) head is ancestor of pending target (parents ∪ graft_parents reachability) — isolate residual other lineages. If none, return head as is. If multiple pending, return with latest UpdatedAt (web "continuing conversation" equivalent).
func (s *LoadSessionService) pendingTailOf(ctx context.Context, repoID, branch string, head domain.ContentHash, preferredSessionID string) domain.ContentHash {
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return head
	}
	best := head
	var bestAt time.Time
	for _, p := range pendings {
		if p.Target == "" || p.Target == head || p.Branch != branch {
			continue
		}
		if _, gerr := s.store.GetSnapshot(ctx, p.Target); gerr != nil {
			continue
		}
		// Session identity outranks branch-head reachability. A PR append can move
		// the branch from H to B while the live uncommitted session remains P→H.
		// Requiring P to reach B drops that live conversation on an immediate
		// restart. This exception is deliberately exact-session only; applying it
		// to merely recent pending pointers would revive stale terminal sessions.
		if preferredSessionID != "" && p.SessionID == preferredSessionID {
			return p.Target
		}
		if !s.reachesFrom(ctx, p.Target, head) {
			continue
		}
		if best == head || p.UpdatedAt.After(bestAt) {
			best = p.Target
			bestAt = p.UpdatedAt
		}
	}
	return best
}

// Determines if the reachability parent from `from` reaches `anc`.
func (s *LoadSessionService) reachesFrom(ctx context.Context, from, anc domain.ContentHash) bool {
	if anc == "" {
		return true
	}
	seen := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		snap, err := s.store.GetSnapshot(ctx, cur)
		if err != nil {
			continue
		}
		stack = append(stack, snap.ReachabilityParents()...)
	}
	return false
}

// Seed budgets are deliberately below provider context windows. Session JSON is
// not token text, so bytes are conservatively estimated at three bytes/token;
// the remaining window is reserved for provider system prompts, repository
// instructions, tool schemas, the next user turn, and model output.
const seedBudgetBytes = 288 << 10 // semantic maximum (Codex profile; mandatory opaque state may overflow it)
const seedDigestBudgetBytes = 72 << 10

func seedBudgets(provider domain.ProviderKind) (total, digest int) {
	switch provider {
	case domain.ProviderCodex:
		return 288 << 10, 72 << 10 // ~96k estimated input tokens
	case domain.ProviderClaude:
		return 192 << 10, 48 << 10 // ~64k estimated input tokens
	default:
		return 192 << 10, 48 << 10
	}
}

func eventsJSONBytes(evs []domain.Event) int {
	total := 0
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		total += len(b)
	}
	return total
}

func isSeedSummaryEvent(ev domain.Event) bool {
	return ev.Kind == domain.EventMessage && ev.Role == "user" && len(ev.Blocks) > 0 &&
		strings.HasPrefix(ev.Blocks[0].Text, seedSummaryPrefix)
}

func isSeedUserBoundary(ev domain.Event) bool {
	return ev.Kind == domain.EventMessage && ev.Role == "user" && !isSeedSummaryEvent(ev)
}

// trimEventsForSeed keeps a bounded recent tail and returns the exact omitted
// events. Normally the cut advances to the next user message, preserving whole
// turns. If the newest in-progress turn alone exceeds the budget, there is no
// next user boundary; in that case the latest user request is anchored in
// front of the bounded suffix. Unmatched tool results at that synthetic gap are
// removed so the provider never receives an output without its call.
func trimEventsForSeed(cir domain.CIRDocument, budget int) (domain.CIRDocument, []domain.Event) {
	return trimEventsForSeedWithKnownCalls(cir, budget, nil)
}

// trimEventsForSeedKeepingPrefix preserves an authoritative provider
// replacement and trims only the suffix accumulated after compaction. If the
// replacement itself exceeds the conversation budget, semantic events fall
// back to the bounded-tail policy while opaque provider compaction state stays
// pinned outside that text budget.
func trimEventsForSeedKeepingPrefix(cir domain.CIRDocument, prefixCount, budget int) (domain.CIRDocument, []domain.Event, bool) {
	if prefixCount == 0 {
		trimmed, omitted := trimEventsForSeed(cir, budget)
		return trimmed, omitted, true
	}
	if prefixCount < 0 || prefixCount > len(cir.Events) {
		trimmed, omitted := trimEventsForSeed(cir, budget)
		return trimmed, omitted, false
	}
	prefix := cir.Events[:prefixCount]
	prefixBytes := eventsJSONBytes(prefix)
	if prefixBytes >= budget {
		// Provider-encrypted compaction state cannot be summarized or sliced. It
		// is mandatory replay state outside the semantic text budget; trim every
		// representable event around it while preserving its original position.
		pinned := make(map[int]bool)
		for i := 0; i < prefixCount; i++ {
			ev := cir.Events[i]
			if ev.Kind == domain.EventCompaction && ev.Replacement == nil && ev.Locked != nil &&
				ev.Locked.Provider == domain.ProviderCodex && ev.Locked.Scheme == "encrypted_content" {
				pinned[i] = true
			}
		}
		trimmed, omitted := trimEventsForSeedWithPinned(cir, budget, nil, pinned)
		return trimmed, omitted, false
	}
	knownCalls := make(map[string]bool)
	for _, ev := range prefix {
		if ev.Kind == domain.EventToolCall && ev.CallID != "" {
			knownCalls[ev.CallID] = true
		}
	}
	tail := cir
	tail.Events = cir.Events[prefixCount:]
	trimmedTail, omitted := trimEventsForSeedWithKnownCalls(tail, budget-prefixBytes, knownCalls)
	out := cir
	out.Events = make([]domain.Event, 0, len(prefix)+len(trimmedTail.Events))
	out.Events = append(out.Events, prefix...)
	out.Events = append(out.Events, trimmedTail.Events...)
	if eventsJSONBytes(out.Events) > budget {
		trimmed, fallbackOmitted := trimEventsForSeed(cir, budget)
		return trimmed, fallbackOmitted, false
	}
	return out, omitted, true
}

func trimEventsForSeedWithKnownCalls(cir domain.CIRDocument, budget int, knownCalls map[string]bool) (domain.CIRDocument, []domain.Event) {
	return trimEventsForSeedWithPinned(cir, budget, knownCalls, nil)
}

func trimEventsForSeedWithPinned(cir domain.CIRDocument, budget int, knownCalls map[string]bool, pinned map[int]bool) (domain.CIRDocument, []domain.Event) {
	evs := cir.Events
	if eventsJSONBytes(evs) <= budget {
		return cir, nil
	}
	// Pinned state is mandatory provider replay state, not semantic transcript.
	// Charging it against this allowance can reduce the semantic budget to zero
	// and split the newest turn even though that turn itself fits the configured
	// replay budget.
	semanticBudget := budget
	sum := 0
	cut := len(evs)
	for i := len(evs) - 1; i >= 0; i-- {
		if pinned[i] {
			continue
		}
		b, _ := json.Marshal(evs[i])
		if sum+len(b) > semanticBudget {
			break
		}
		sum += len(b)
		cut = i
	}

	selected := make([]bool, len(evs))
	nextUser := -1
	for i := cut; i < len(evs); i++ {
		if isSeedUserBoundary(evs[i]) {
			nextUser = i
			break
		}
	}

	if nextUser >= 0 {
		for i := nextUser; i < len(evs); i++ {
			selected[i] = true
		}
	} else {
		latestUser := -1
		for i := cut - 1; i >= 0; i-- {
			if isSeedUserBoundary(evs[i]) {
				latestUser = i
				break
			}
		}
		if latestUser < 0 {
			for i := cut; i < len(evs); i++ {
				selected[i] = true
			}
		} else {
			selected[latestUser] = true
			userBytes, _ := json.Marshal(evs[latestUser])
			remaining := semanticBudget - len(userBytes)
			suffixCut := len(evs)
			suffixBytes := 0
			if remaining > 0 {
				for i := len(evs) - 1; i > latestUser; i-- {
					if pinned[i] {
						continue
					}
					b, _ := json.Marshal(evs[i])
					if suffixBytes+len(b) > remaining {
						break
					}
					suffixBytes += len(b)
					suffixCut = i
				}
			}
			for i := suffixCut; i < len(evs); i++ {
				selected[i] = true
			}
		}
	}
	for i := range pinned {
		if i >= 0 && i < len(selected) {
			selected[i] = true
		}
	}

	// Any bounded suffix can begin after a tool call but before its result.
	// Keep only results whose call is also present in the selected stream.
	calls := make(map[string]bool, len(knownCalls))
	for callID := range knownCalls {
		calls[callID] = true
	}
	for i, keep := range selected {
		if keep && evs[i].Kind == domain.EventToolCall && evs[i].CallID != "" {
			calls[evs[i].CallID] = true
		}
	}
	for i, keep := range selected {
		if keep && evs[i].Kind == domain.EventToolResult && evs[i].CallID != "" && !calls[evs[i].CallID] {
			selected[i] = false
		}
	}

	out := cir
	out.Events = make([]domain.Event, 0, len(evs))
	omitted := make([]domain.Event, 0, len(evs))
	for i, ev := range evs {
		if selected[i] {
			out.Events = append(out.Events, ev)
		} else {
			omitted = append(omitted, ev)
		}
	}
	if len(omitted) == 0 {
		return cir, nil
	}
	return out, omitted
}

func truncateUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

func truncateUTF8Tail(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const marker = "[... earlier summary omitted ...]\n"
	if maxBytes <= len(marker) {
		return truncateUTF8Prefix(marker, maxBytes)
	}
	start := len(s) - (maxBytes - len(marker))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return marker + s[start:]
}

func renderSeedBulletSection(title string, items []string, maxBytes int) string {
	header := "\n" + title + "\n"
	if len(items) == 0 || maxBytes <= len(header)+2 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		line := "- " + item + "\n"
		remaining := maxBytes - b.Len()
		if remaining <= 2 {
			break
		}
		if len(line) > remaining {
			b.WriteString("- " + truncateUTF8Prefix(item, remaining-2))
			break
		}
		b.WriteString(line)
	}
	if b.Len() == len(header) {
		return ""
	}
	return b.String()
}

func renderSeedDigest(digest domain.MemoryDigest, dropped, budget int) string {
	header := fmt.Sprintf("%s %d events were omitted to fit the context window; below is a bounded memory summary of the omitted span. The recent history follows verbatim.\n", seedSummaryPrefix, dropped)
	return renderSeedDigestWithHeader(digest, header, budget)
}

func renderPortableReplayDigest(digest domain.MemoryDigest, budget int) string {
	header := seedSummaryPrefix + " Portable memory from the source context is included below. The full source conversation follows verbatim.\n"
	return renderSeedDigestWithHeader(digest, header, budget)
}

func renderSeedDigestWithHeader(digest domain.MemoryDigest, header string, budget int) string {
	digest = domain.PromptStructuredProjection(digest)
	if len(header) >= budget {
		return truncateUTF8Prefix(header, budget)
	}

	facts := renderSeedBulletSection("Key facts:", seedWorthyFacts(digest.KeyFacts), budget/6)
	taskHeading := seedLegacyTasksHeading
	if digest.TasksAuthoritative {
		taskHeading = seedAuthoritativeTasksHeading
	}
	tasks := renderSeedBulletSection(taskHeading, digest.OpenTasks, budget/4)
	if digest.TasksAuthoritative && tasks == "" && budget/4 > len(taskHeading) {
		// Preserve an explicit all-tasks-completed tombstone if this prompt is
		// later the sole recoverable cxt seed. Unmarked legacy headings are never
		// upgraded to authority.
		tasks = taskHeading + "\n"
	}
	remaining := budget - len(header) - len(facts) - len(tasks)

	summaryBlock := ""
	if summary := strings.TrimSpace(digest.Summary); summary != "" && remaining > 2 {
		summary = truncateUTF8Tail(summary, remaining-2)
		summaryBlock = "\n" + summary + "\n"
	}
	out := header + summaryBlock + facts + tasks
	if len(out) > budget {
		return truncateUTF8Prefix(out, budget)
	}
	return out
}

// portableReplaySeed projects memory that is not already represented by the
// verbatim current conversation. A full-fidelity materialization still starts
// a new provider session, so provider/native baselines and inherited lineage
// memory must cross that boundary unless the target proves it auto-loads the
// exact bytes. The immutable stored digest remains untouched.
func (s *LoadSessionService) portableReplaySeed(
	ctx context.Context,
	seed domain.CIRDocument,
	snap domain.Snapshot,
	target domain.ProviderKind,
	cwd string,
	budget int,
) (domain.Event, bool) {
	digest, ok := snapshotMemoryProjection(ctx, s.store, snap)
	if !ok {
		return domain.Event{}, false
	}
	// Normalize legacy opaque digests before prompt-only projections. Besides
	// attaching provenance, this removes the private conversation-delta marker
	// from provider-visible rendering.
	digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	digest = domain.WithoutConversationDeltaFromSource(digest, snap.ID)
	replayedUsers, replayedAssistants := replayedConversationTexts(seed.Events)
	digest = domain.WithoutExactReplayedConversationItems(digest, replayedUsers, replayedAssistants)
	for _, event := range seed.Events {
		if !event.CompactSummary || isSeedSummaryEvent(event) {
			continue
		}
		for _, block := range event.Blocks {
			digest = domain.WithoutExactReplayedSummary(digest, block.Text)
		}
	}
	digest = boundCarriedDigest(digest)
	if carried, found := syntheticSeedMemoryProjection(seed.Events, snap.ID); found {
		// A materialized seed can be the only remaining representation of older
		// context even when the snapshot also has a narrower stored digest. Fold
		// it into the normalized replacement before removing the old event.
		digest = domain.MergeDigests(carried, digest)
		digest = boundCarriedDigest(digest)
	}
	if native, found := readTargetNativeMemory(
		ctx, s.memSources, target, cwd, seed.Envelope.SessionOriginID,
	); found {
		digest = projectAutoLoadedNative(digest, native)
	}
	if !memoryDigestHasProjection(digest) {
		return domain.Event{}, false
	}
	return domain.Event{
		Kind:           domain.EventMessage,
		Role:           "user",
		CompactSummary: true,
		Blocks: []domain.ContentBlock{{
			Type: "text",
			Text: renderPortableReplayDigest(digest, budget),
		}},
	}, true
}

func replayedConversationTexts(events []domain.Event) (users, assistants []string) {
	for _, event := range events {
		if event.Kind != domain.EventMessage || event.CompactSummary || isSeedSummaryEvent(event) {
			continue
		}
		parts := make([]string, 0, len(event.Blocks))
		for _, block := range event.Blocks {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, strings.TrimSpace(block.Text))
			}
		}
		text := strings.Join(parts, "\n")
		if text == "" {
			continue
		}
		switch event.Role {
		case "user":
			users = append(users, text)
		case "assistant":
			assistants = append(assistants, text)
		}
	}
	return users, assistants
}

func replaceSyntheticSeedSummary(events []domain.Event, seed domain.Event, insertAfter int) []domain.Event {
	if insertAfter < 0 || insertAfter > len(events) {
		insertAfter = 0
	}
	out := make([]domain.Event, 0, len(events)+1)
	for i, event := range events {
		if i == insertAfter {
			out = append(out, seed)
		}
		if isSeedSummaryEvent(event) {
			continue
		}
		out = append(out, event)
	}
	if insertAfter == len(events) {
		out = append(out, seed)
	}
	seedAt := -1
	for i := range out {
		if out[i].CompactSummary && isSeedSummaryEvent(out[i]) {
			seedAt = i
			break
		}
	}
	if seedAt >= 0 {
		switch {
		case seedAt+1 < len(out):
			out[seedAt].Ts = out[seedAt+1].Ts
		case seedAt > 0:
			out[seedAt].Ts = out[seedAt-1].Ts
		}
	}
	for i := range out {
		out[i].Seq = i
	}
	return out
}

// `seedSummaryPrefix` is the identifier prefix for seed compression summary events — a marker to find and replace (remove) the previous generation summary in the tail when the next seed is generated. It is removed from materialized copies, while the saved doc remains unchanged.
const seedSummaryPrefix = "[cxt] This session was resumed from a branch context seed."

const (
	seedLegacyTasksHeading        = "Open tasks:"
	seedAuthoritativeTasksHeading = "Open tasks: <!-- cxt:tasks-authoritative-v1 -->"
)

// prependTrimDigest inserts a CompactSummary event for the omitted history before the recent raw tail.
// This preserves decisions, constraints, and open tasks within the seed budget. The compact marker drives
// the viewer's ◈ rendering and last-write-wins carry-over during memory distillation.
//
// Summary source priority: The digest on this snapshot, or the deterministic
// projection of nearest digests across every parent lineage, is first priority.
// In-place omitted-span distillation supplements that project memory. It is
// mandatory whenever events were trimmed; stored memory cannot prove that it
// contains the exact current omitted span.
//
// Deduplication:
//   - Previous generation seed summary (prefix CompactSummary) in the tail is removed — the new summary takes precedence.
//   - An exact working-tree-scoped native baseline (e.g. Claude MEMORY.md) is
//     omitted from the prompt copy because the target provider loads it again.
//     Session-scoped memory remains portable across the new provider session ID.
//   - KeyFacts noise such as whitespace-free tool tokens and legacy ingestion markers ("native memory:"/"absorbed from") is excluded from seed content.
func (s *LoadSessionService) prependTrimDigest(ctx context.Context, omitted, seed domain.CIRDocument, snap domain.Snapshot, target domain.ProviderKind, cwd string) (domain.CIRDocument, error) {
	out, _, _, err := s.prependTrimDigestWithStatus(ctx, omitted, seed, snap, target, cwd, nil)
	return out, err
}

// prependTrimDigestWithStatus reports both whether a new bounded digest was
// synthesized and whether it safely supersedes prior cxt seeds. Callers must
// retain an old seed unless its meaning came from stored memory or was folded
// into a normalized projection; otherwise that seed can be the sole surviving
// representation of earlier context.
func (s *LoadSessionService) prependTrimDigestWithStatus(ctx context.Context, omitted, seed domain.CIRDocument, snap domain.Snapshot, target domain.ProviderKind, cwd string, priorSeeds []domain.Event) (domain.CIRDocument, bool, bool, error) {
	digest, err := s.distiller.Distill(ctx, omitted, nil)
	if err != nil {
		return seed, false, false, err
	}
	digest.SnapshotID = snap.ID
	if digest.SnapshotID != "" {
		// Normalize the private version marker into provenance fragments before
		// rendering provider-visible prompt text.
		digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	}
	// Prioritize the stored memorize digest (self-snapshot → parent projection).
	stored, hasStored := domain.MemoryDigest{}, false
	if snap.MemoryHash != "" {
		if d, ok := snapshotMemoryProjection(ctx, s.store, snap); ok {
			stored, hasStored = d, true
		}
	}
	if !hasStored {
		if d, ok := ancestorMemoryProjection(ctx, s.store, snap); ok {
			stored, hasStored = d, true
		}
	}
	storedUsable := false
	if hasStored {
		// This snapshot's recent conversation is present in the raw replay tail
		// (or re-distilled from the exact omitted slice below). Retain provider or
		// native baseline memory, but do not inject that same source delta twice.
		stored = domain.WithoutConversationDeltaFromSource(stored, snap.ID)
		// Stored memory is immutable archival state and can contain a full native
		// memory or legacy cxt seed generations. Loading it into a provider prompt
		// is an inherited projection, so apply the same recursion filter and carry
		// bounds used by branch/memorize inheritance. The original object remains
		// untouched and the fresh digest below still contributes recent conversation.
		stored = boundCarriedDigest(stored)
		storedUsable = memoryDigestHasProjection(stored)
		if storedUsable {
			digest = domain.MergeDigests(stored, digest)
		}
	}
	seedProjected := false
	if !storedUsable {
		if carried, ok := syntheticSeedMemoryProjection(priorSeeds, snap.ID); ok {
			digest = domain.MergeDigests(carried, digest)
			seedProjected = true
		}
	}
	// Stored memory remains untouched. Only the provider-visible copy may drop
	// a baseline that the target guarantees it will load for this working tree.
	if native, ok := readTargetNativeMemory(ctx, s.memSources, target, cwd, omitted.Envelope.SessionOriginID); ok {
		digest = projectAutoLoadedNative(digest, native)
	}
	_, digestBudget := seedBudgets(target)
	text := renderSeedDigest(digest, len(omitted.Events), digestBudget)
	ev := domain.Event{Kind: domain.EventMessage, Role: "user", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
	// Remove previous generation seed summary (materialized copy limit): Since the new summary inherits its content,
	// leaving it would cause ◈ blocks to accumulate per generation. Determination is based on prefix text — legacy seed messages (unmarked user) must also be removed.
	tail := make([]domain.Event, 0, len(seed.Events))
	for _, e := range seed.Events {
		if isSeedSummaryEvent(e) {
			continue
		}
		tail = append(tail, e)
	}
	if len(tail) > 0 {
		ev.Ts = tail[0].Ts
		ev.Seq = tail[0].Seq - 1
	}
	out := seed
	out.Events = append([]domain.Event{ev}, tail...)
	return out, true, len(priorSeeds) == 0 || storedUsable || seedProjected, nil
}

func memoryDigestHasProjection(d domain.MemoryDigest) bool {
	return strings.TrimSpace(d.Summary) != "" || len(d.KeyFacts) > 0 || len(d.OpenTasks) > 0
}

func (s *LoadSessionService) insertTrimDigestAfterPrefix(ctx context.Context, omitted, seed domain.CIRDocument, prefixCount int, snap domain.Snapshot, target domain.ProviderKind, cwd string) (domain.CIRDocument, error) {
	if prefixCount < 0 || prefixCount > len(seed.Events) {
		return s.prependTrimDigest(ctx, omitted, seed, snap, target, cwd)
	}
	prefix := append([]domain.Event{}, seed.Events[:prefixCount]...)
	priorSeeds := make([]domain.Event, 0, 1)
	for _, ev := range prefix {
		if isSeedSummaryEvent(ev) {
			priorSeeds = append(priorSeeds, ev)
		}
	}
	tail := seed
	tail.Events = seed.Events[prefixCount:]
	var inserted, replacesPriorSeeds bool
	var err error
	tail, inserted, replacesPriorSeeds, err = s.prependTrimDigestWithStatus(ctx, omitted, tail, snap, target, cwd, priorSeeds)
	if err != nil {
		return seed, err
	}
	if inserted && replacesPriorSeeds {
		// A prior cxt seed is semantic projection, not provider-owned replay
		// state. Keeping it beside the replacement digest recursively embeds
		// every earlier seed generation and was measured at 40 nested copies in
		// one Codex replay. Real messages and opaque compaction state remain
		// pinned; only the successfully superseded synthetic projection is
		// removed.
		filtered := prefix[:0]
		for _, ev := range prefix {
			if isSeedSummaryEvent(ev) {
				continue
			}
			filtered = append(filtered, ev)
		}
		prefix = filtered
	}
	out := seed
	out.Events = make([]domain.Event, 0, len(prefix)+len(tail.Events))
	out.Events = append(out.Events, prefix...)
	out.Events = append(out.Events, tail.Events...)
	for i := range out.Events {
		out.Events[i].Seq = i
	}
	return out, nil
}

// syntheticSeedMemoryProjection makes a bounded carried digest from legacy
// cxt seed text when no attached/ancestor memory exists. The first header is
// replaced by the new seed's header, while embedded headers are neutralized so
// the next load cannot recognize and recursively re-ingest generations. Raw
// seed events remain immutable in their source snapshots.
func syntheticSeedMemoryProjection(events []domain.Event, source domain.ContentHash) (domain.MemoryDigest, bool) {
	if source == "" {
		return domain.MemoryDigest{}, false
	}
	var summaries []string
	for _, ev := range events {
		if !isSeedSummaryEvent(ev) {
			continue
		}
		text := strings.TrimSpace(ev.Blocks[0].Text)
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		} else {
			text = ""
		}
		text = strings.ReplaceAll(text, seedSummaryPrefix, "[prior cxt context]")
		text = strings.ReplaceAll(text, "[cxt seed] Branch-switch context:", "[prior branch context]")
		if text = strings.TrimSpace(text); text != "" {
			summaries = append(summaries, text)
		}
	}
	if len(summaries) == 0 {
		return domain.MemoryDigest{}, false
	}
	summary := strings.Join(summaries, "\n\n")
	facts, tasks, tasksAuthoritative := syntheticSeedStructuredProjection(summary)
	summary = withoutSyntheticSeedStructuredSections(summary)
	return boundCarriedDigest(domain.MemoryDigest{
		SnapshotID:         source,
		Summary:            summary,
		KeyFacts:           facts,
		OpenTasks:          tasks,
		TasksAuthoritative: tasksAuthoritative,
	}), true
}

func withoutSyntheticSeedStructuredSections(text string) string {
	const (
		sectionNone = iota
		sectionFacts
		sectionTasks
	)
	section := sectionNone
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "Key facts:":
			section = sectionFacts
			continue
		case seedLegacyTasksHeading, seedAuthoritativeTasksHeading:
			section = sectionTasks
			continue
		}
		if section != sectionNone {
			if trimmed == "" || strings.HasPrefix(trimmed, "- ") {
				continue
			}
			section = sectionNone
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// syntheticSeedStructuredProjection recovers the newest rendered cxt bullet
// sections before narrative tail truncation. These headings are emitted by
// renderSeedDigest itself, so exact heading matching avoids interpreting
// arbitrary prose as structure.
func syntheticSeedStructuredProjection(text string) (facts, tasks []string, tasksAuthoritative bool) {
	const (
		sectionNone = iota
		sectionFacts
		sectionTasks
	)
	section := sectionNone
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case "Key facts:":
			facts = nil // latest rendered section wins
			section = sectionFacts
			continue
		case seedLegacyTasksHeading:
			tasks = nil // latest legacy section wins, but remains unattested
			tasksAuthoritative = false
			section = sectionTasks
			continue
		case seedAuthoritativeTasksHeading:
			tasks = nil // an empty marked section is an explicit tombstone
			tasksAuthoritative = true
			section = sectionTasks
			continue
		}
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			section = sectionNone
			continue
		}
		item := strings.TrimSpace(trimmed[2:])
		if item == "" {
			continue
		}
		switch section {
		case sectionFacts:
			facts = append(facts, item)
		case sectionTasks:
			tasks = append(tasks, item)
		}
	}
	return facts, tasks, tasksAuthoritative
}

// seedWorthyFacts retains only KeyFacts that are worth placing in the seed header. The distillation now
// extracts sentence-form facts from compressed summaries, but legacy stored digests may contain tool-name lists and ingestion markers (empirically observed: "apply_patch", "unknown:Agent", "native memory: …") —
// excluding whitespace-free single tokens and marker prefixes, only sentence-form facts pass.
func seedWorthyFacts(facts []string) []string {
	return domain.PromptWorthyMemoryFacts(facts)
}

// loadMemory performs memory-form restoration: native-first ingestion → distillation → provider memory file injection.
func (s *LoadSessionService) loadMemory(ctx context.Context, cir domain.CIRDocument, snap domain.Snapshot, target domain.ProviderKind, cwd string) (inbound.LoadOutput, error) {
	targetNative, _ := readTargetNativeMemory(ctx, s.memSources, target, cwd, cir.Envelope.SessionOriginID)
	var distillNative *domain.NativeMemory
	if cir.Envelope.SourceProvider == "" || target == cir.Envelope.SourceProvider {
		distillNative = targetNative
	}
	digest, err := s.distiller.Distill(ctx, cir.EffectiveContext(), distillNative)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	digest.SnapshotID = snap.ID
	digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	// Heritage continuation (same logic as memorize): merge project memory from
	// every parent lineage, including overlay grafts.
	if prior, ok := priorMemoryProjection(ctx, s.store, snap); ok {
		prior = boundCarriedDigest(prior)
		digest = domain.MergeDigests(prior, digest)
	}
	digest = projectAutoLoadedNative(digest, targetNative)
	if digest.Provider == "" {
		digest.Provider = target
	}
	sink, ok := s.memSinks[target]
	if !ok {
		return inbound.LoadOutput{}, domain.ErrUnsupportedProvider
	}
	path, err := sink.Inject(ctx, digest, cwd)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	return inbound.LoadOutput{WrittenPath: path, Fidelity: domain.FidelityMemory}, nil
}

// Ensure LoadSessionService implements inbound.LoadSession.
var _ inbound.LoadSession = (*LoadSessionService)(nil)
