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
//     c. LoadOutput{WrittenPath: sessionPath, ResumeCmd, Fidelity: full/reconstructed}
//     d. **Fallback to memory mode automatically on Materialize failure** (Fidelity downgrade → memory).
//     memory:
//     a. MemorySource[TargetProvider].ReadNative(Cwd) → (native, found)
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
		snapID = s.pendingTailOf(ctx, in.RepoID, in.Ref, snapID)
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
	// Recent event tail remains within budget — truncation point is user prompt boundary.
	seedCIR := cir
	var omitted []domain.Event
	if eventsJSONBytes(cir.Events) > seedBudgetBytes {
		seedCIR, omitted = trimEventsForSeed(cir, seedTailBudgetBytes)
	}
	dropped := len(omitted)
	if dropped > 0 {
		// Omit memory digest of skipped segment at seed start with "CompactSummary" event — context compression equivalent (summary + recent raw). Viewer collapses, distiller 1st priority last-wins (memory ↔ context cycle accumulation block).
		// Failure is fail-open (tail only).
		omittedCIR := cir
		omittedCIR.Events = omitted
		seedCIR = s.prependTrimDigest(ctx, omittedCIR, seedCIR, snap, target, in.Cwd)
	}
	// A restored session belongs to the working tree it is being restored into,
	// not the machine/path where the source snapshot was captured. Codex active
	// session discovery reads payload.cwd, so retaining the source path makes a
	// relocated clone impossible to capture after resume.
	if targetCwd := materializationCwd(in.Cwd); targetCwd != "" {
		seedCIR.Envelope.Cwd = targetCwd
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
func (s *LoadSessionService) pendingTailOf(ctx context.Context, repoID, branch string, head domain.ContentHash) domain.ContentHash {
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

// `seedBudgetBytes` is the budget for the event tail of full materialization. Approximately 100k~130k tokens —
// Claude(200k)/Codex(258k) windows leave room for default instructions and buffer.
const seedBudgetBytes = 400 << 10

// A bounded digest gets a fixed share of the total event budget. Without this
// reservation, a large inherited digest can dwarf the raw-tail limit and push
// the materialized session over the target model's context window.
const seedDigestBudgetBytes = 96 << 10
const seedTailBudgetBytes = seedBudgetBytes - seedDigestBudgetBytes

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
	evs := cir.Events
	if eventsJSONBytes(evs) <= budget {
		return cir, nil
	}
	sum := 0
	cut := len(evs)
	for i := len(evs) - 1; i >= 0; i-- {
		b, _ := json.Marshal(evs[i])
		if sum+len(b) > budget {
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
			remaining := budget - len(userBytes)
			suffixCut := len(evs)
			suffixBytes := 0
			if remaining > 0 {
				for i := len(evs) - 1; i > latestUser; i-- {
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

	// Any bounded suffix can begin after a tool call but before its result.
	// Keep only results whose call is also present in the selected stream.
	calls := map[string]bool{}
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
	if len(header) >= budget {
		return truncateUTF8Prefix(header, budget)
	}

	facts := renderSeedBulletSection("Key facts:", seedWorthyFacts(digest.KeyFacts), budget/6)
	tasks := renderSeedBulletSection("Open tasks:", digest.OpenTasks, budget/4)
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

// `seedSummaryPrefix` is the identifier prefix for seed compression summary events — a marker to find and replace (remove) the previous generation summary in the tail when the next seed is generated. It is removed from materialized copies, while the saved doc remains unchanged.
const seedSummaryPrefix = "[cxt] This session was resumed from a branch context seed."

// prependTrimDigest inserts a CompactSummary event for the omitted history before the recent raw tail.
// This preserves decisions, constraints, and open tasks within the seed budget. The compact marker drives
// the viewer's ◈ rendering and last-write-wins carry-over during memory distillation.
//
// Summary source priority: The digest (this snapshot's MemoryHash, or the closest ancestor's if not memorized) is 1st priority — it is copied directly from the memory being uploaded to the DB. In-place head distillation is a supplement to the stored digest, and the two sources are deterministically merged. If both fail, fail-open (seed with only the tail).
//
// Deduplication:
//   - Previous generation seed summary (prefix CompactSummary) in the tail is removed — the new summary takes precedence.
//   - If the digest summary matches the original native memory (e.g., MEMORY.md) of the target provider, it is omitted — the agent loads context itself at session start, so context re-injection is redundant.
//   - KeyFacts noise such as whitespace-free tool tokens and legacy ingestion markers ("native memory:"/"absorbed from") is excluded from seed content.
func (s *LoadSessionService) prependTrimDigest(ctx context.Context, omitted, seed domain.CIRDocument, snap domain.Snapshot, target domain.ProviderKind, cwd string) domain.CIRDocument {
	digest, derr := s.distiller.Distill(ctx, omitted, nil)
	if derr != nil {
		digest = domain.MemoryDigest{} // Send the stored digest even if empty
	}
	// Prioritize the stored memorize digest (self-snapshot → closest ancestor).
	stored, hasStored := domain.MemoryDigest{}, false
	if snap.MemoryHash != "" {
		if d, gerr := s.store.GetMemory(ctx, snap.MemoryHash); gerr == nil {
			stored, hasStored = d, true
		}
	}
	if !hasStored {
		if d, ok := nearestAncestorDigest(ctx, s.store, snap); ok {
			stored, hasStored = d, true
		}
	}
	if hasStored {
		digest = domain.MergeDigests(stored, digest)
	}
	if derr != nil && !hasStored {
		return seed
	}
	// Skip native-memory duplicates when the summary repeats the ingested native memory verbatim.
	if src, ok := s.memSources[target]; ok && digest.Summary != "" {
		if nm, found, _ := src.ReadNative(ctx, cwd); found &&
			strings.TrimSpace(digest.Summary) == strings.TrimSpace(nm.Text) {
			digest.Summary = ""
		}
	}
	text := renderSeedDigest(digest, len(omitted.Events), seedDigestBudgetBytes)
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
	return out
}

// seedWorthyFacts retains only KeyFacts that are worth placing in the seed header. The distillation now
// extracts sentence-form facts from compressed summaries, but legacy stored digests may contain tool-name lists and ingestion markers (empirically observed: "apply_patch", "unknown:Agent", "native memory: …") —
// excluding whitespace-free single tokens and marker prefixes, only sentence-form facts pass.
func seedWorthyFacts(facts []string) []string {
	var out []string
	for _, f := range facts {
		t := strings.TrimSpace(f)
		if t == "" || !strings.Contains(t, " ") ||
			strings.HasPrefix(t, "native memory:") || strings.HasPrefix(t, "absorbed from") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// loadMemory performs memory-form restoration: native-first ingestion → distillation → provider memory file injection.
func (s *LoadSessionService) loadMemory(ctx context.Context, cir domain.CIRDocument, snap domain.Snapshot, target domain.ProviderKind, cwd string) (inbound.LoadOutput, error) {
	var native *domain.NativeMemory
	if src, ok := s.memSources[target]; ok {
		if nm, found, _ := src.ReadNative(ctx, cwd); found {
			native = &nm
		}
	}
	digest, err := s.distiller.Distill(ctx, cir, native)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	// Heritage continuation (same logic as memorize): Merge the closest ancestor digest to inherit —
	// appending the context to memory mode loads the previous context's memory even if the context is merged.
	if prior, ok := nearestAncestorDigest(ctx, s.store, snap); ok {
		prior = boundCarriedDigest(prior)
		digest = domain.MergeDigests(prior, digest)
	}
	digest.SnapshotID = snap.ID
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
