package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// claude distillation empirical form (cumulative version): Previous generation summary + latest summary coexist in one text.
// Extraction should use only the "last" occurrence of each section.
const structuredSummary = `This session is being continued from a previous conversation.

Summary:
1. Primary Request and Intent:
   old intent
2. Key Technical Concepts:
   - stale fact from older generation
7. Pending Tasks:
   - stale task

This session is being continued from a previous conversation.

Summary:
1. Primary Request and Intent:
   The user is dogfooding cxthub.
2. Key Technical Concepts:
   - **Overlay graft**: appendDiverged never rewrites Parents; reachability = Parents ∪ GraftParents.
   - Reflog: append-only ref-move log (FS reflog.jsonl / PG reflog table 0028).
     nested detail line that must not become its own fact
3. Files and Code Sections:
   - backend/internal/domain/entities.go — ReachabilityParents()
4. Errors and fixes:
   - zsh word-split issue → python3 script.
5. Problem Solving:
   - Root-caused sync-conflict saga.
6. All user messages:
   - "continue"
7. Pending Tasks:
   - **(active)** Fix empty [reasoning] viewer rendering.
   - Graph 3-tier: Push - No Push - No Commit.
8. Current Work:
   Immediately before this summary...`

func distillWithSummary(t *testing.T, summary string) domain.MemoryDigest {
	t.Helper()
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderClaude
	cir.Events = []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "do work"}}},
		{Kind: domain.EventToolCall, ToolName: "apply_patch"},
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true,
			Blocks: []domain.ContentBlock{{Type: "text", Text: summary}}},
	}
	d, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDistillExtractsStructuredFacts fixes distillation extraction:
// KeyFacts ← "Key Technical Concepts" top-level bullet (last occurrence, emphasis markers removed),
// OpenTasks ← "Pending Tasks" bullet. Tool name is not mixed with KeyFacts.
func TestDistillExtractsStructuredFacts(t *testing.T) {
	d := distillWithSummary(t, structuredSummary)

	if len(d.KeyFacts) != 2 {
		t.Fatalf("KeyFacts = %d items %v (want 2 — latest generation top-level bullet only)", len(d.KeyFacts), d.KeyFacts)
	}
	if !strings.HasPrefix(d.KeyFacts[0], "Overlay graft:") {
		t.Fatalf("emphasis marker not removed/ordering issue: %q", d.KeyFacts[0])
	}
	for _, f := range d.KeyFacts {
		if strings.Contains(f, "stale") {
			t.Fatalf("previous generation section mixed: %q", f)
		}
		if f == "apply_patch" {
			t.Fatal("tool name mixed with KeyFacts")
		}
		if strings.Contains(f, "nested detail") {
			t.Fatalf("Nested detail extracted as fact: %q", f)
		}
	}
	if len(d.OpenTasks) != 2 || !strings.Contains(d.OpenTasks[0], "[reasoning]") {
		t.Fatalf("OpenTasks extraction failed: %v", d.OpenTasks)
	}
	if !d.TasksAuthoritative {
		t.Fatal("Structural extraction successful but TasksAuthoritative not set — ancestor completed tasks are inherited in merge")
	}
	for _, task := range d.OpenTasks {
		if strings.Contains(task, "stale") {
			t.Fatalf("Previous generation task mixed: %q", task)
		}
	}
}

// TestDistillUnstructuredSummaryFallsBack ensures that the existing behavior (tool name fallback, no OpenTasks) is preserved.
func TestDistillUnstructuredSummaryFallsBack(t *testing.T) {
	d := distillWithSummary(t, "just a plain free-form summary without numbered sections")

	if len(d.KeyFacts) != 1 || d.KeyFacts[0] != "apply_patch" {
		t.Fatalf("Unstructured fallback broken: %v", d.KeyFacts)
	}
	if len(d.OpenTasks) != 0 {
		t.Fatalf("OpenTasks appears in unstructured summary: %v", d.OpenTasks)
	}
	if d.TasksAuthoritative {
		t.Fatal("Unstructured summary but TasksAuthoritative set")
	}
}

// TestDistillNoProvenanceMarkerInFacts ensures that the source marker ("native memory: …") does not mix with KeyFacts — the source is not a fact (review P2). Native memory is loaded by the agent at session start, so it has no information value.
func TestDistillNoProvenanceMarkerInFacts(t *testing.T) {
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderClaude
	cir.Events = []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true,
			Blocks: []domain.ContentBlock{{Type: "text", Text: structuredSummary}}},
	}
	native := &domain.NativeMemory{Source: "claude:MEMORY.md", Text: "memory body"}
	d, err := NewRuleDistiller().Distill(context.Background(), cir, native)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.KeyFacts {
		if strings.HasPrefix(f, "native memory:") {
			t.Fatalf("source marker mixed with KeyFacts: %q", f)
		}
	}
}

func TestDistillExtractiveFallbackPreservesRecentIntentAndOutcomes(t *testing.T) {
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderCodex
	cir.Envelope.CompactionCount = 2 // plaintext message unavailable (encrypted provider payload)
	cir.Events = []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "[cxt seed] Branch-switch context: main → fix/x\n" + strings.Repeat("old seed ", 5000)}}},
		{Kind: domain.EventToolCall, ToolName: "apply_patch"},
		{Kind: domain.EventToolResult, Output: "noisy tool output"},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Find why compacted memory is lost."}}},
		{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "Chunk storage is lossless; the plaintext compaction message is empty."}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Fix it without decrypting provider content."}}},
		{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "Use a deterministic extractive fallback from preserved CIR."}}},
	}

	d, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Recent user intent",
		"Find why compacted memory is lost.",
		"Fix it without decrypting provider content.",
		"Recent assistant outcomes",
		"Chunk storage is lossless",
		"deterministic extractive fallback",
	} {
		if !strings.Contains(d.Summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, d.Summary)
		}
	}
	for _, unwanted := range []string{"[cxt seed]", "apply_patch", "noisy tool output", "tools ["} {
		if strings.Contains(d.Summary, unwanted) {
			t.Fatalf("summary contains noise %q:\n%s", unwanted, d.Summary)
		}
	}
	if len([]rune(d.Summary)) > extractiveDigestMaxRunes {
		t.Fatalf("summary exceeds bound: %d", len([]rune(d.Summary)))
	}
	if len(d.KeyFacts) != 0 {
		t.Fatalf("tool names leaked into key facts: %v", d.KeyFacts)
	}
}

func TestDistillExtractiveFallbackIsRecentBoundedAndDeterministic(t *testing.T) {
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderCodex
	for i := 0; i < 20; i++ {
		cir.Events = append(cir.Events,
			domain.Event{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("request-%02d %s", i, strings.Repeat("x", 3000))}}},
			domain.Event{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("outcome-%02d %s", i, strings.Repeat("y", 3000))}}},
		)
	}
	first, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary != second.Summary {
		t.Fatal("extractive fallback is not deterministic")
	}
	if strings.Contains(first.Summary, "request-00") || !strings.Contains(first.Summary, "request-19") {
		t.Fatalf("fallback did not retain the recent bounded window")
	}
	if len([]rune(first.Summary)) > extractiveDigestMaxRunes {
		t.Fatalf("summary exceeds bound: %d", len([]rune(first.Summary)))
	}
}

// TestExtractiveDigestSkipsResumeSeedAndEnvironmentContext (#32): resume-seed
// boilerplate loses its CompactSummary marking after materialize→re-capture and
// harness environment_context blocks are machine state — neither may consume
// the bounded per-role slots of the extractive fallback.
func TestExtractiveDigestSkipsResumeSeedAndEnvironmentContext(t *testing.T) {
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderCodex
	cir.Events = []domain.Event{
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text",
			Text: "[cxt] This session was resumed from a branch context seed. 601 older events were omitted." + strings.Repeat(" boilerplate", 300)}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text",
			Text: "<environment_context>\n<cwd>/tmp/x</cwd>\n</environment_context>"}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Fix findings one and two."}}},
		{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "Budgeted the seed prompt sections."}}},
	}
	d, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fix findings one and two.", "Budgeted the seed prompt sections."} {
		if !strings.Contains(d.Summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, d.Summary)
		}
	}
	for _, unwanted := range []string{"This session was resumed from a branch context seed", "<environment_context>"} {
		if strings.Contains(d.Summary, unwanted) {
			t.Fatalf("summary contains noise %q:\n%s", unwanted, d.Summary)
		}
	}
}

// TestDistillDoesNotPromoteSeedDigestAsAgentCompaction (#38): cxt-synthesized
// seed digests carry the CompactSummary marking but are bounded copies of
// inherited memory. They must not be selected as the agent's own compression
// summary; a real agent summary must still win when both are present.
func TestDistillDoesNotPromoteSeedDigestAsAgentCompaction(t *testing.T) {
	seedText := "[cxt] This session was resumed from a branch context seed. 2819 older events were omitted." + strings.Repeat(" inherited", 200)
	cir := domain.CIRDocument{}
	cir.Envelope.SourceProvider = domain.ProviderClaude
	cir.Events = []domain.Event{
		{Kind: domain.EventMessage, Role: "user", CompactSummary: true,
			Blocks: []domain.ContentBlock{{Type: "text", Text: seedText}}},
		{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Relabel the seed summary rows."}}},
		{Kind: domain.EventMessage, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "Excluded seed digests from the priority path."}}},
	}
	d, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Summary, "resumed from a branch context seed") {
		t.Fatalf("seed digest promoted to agent compaction summary:\n%s", d.Summary[:200])
	}
	if !strings.Contains(d.Summary, "Relabel the seed summary rows.") {
		t.Fatalf("extractive fallback missing real conversation:\n%s", d.Summary)
	}

	agent := structuredSummary
	cir.Events = append(cir.Events, domain.Event{Kind: domain.EventMessage, Role: "user", CompactSummary: true,
		Blocks: []domain.ContentBlock{{Type: "text", Text: agent}}})
	d2, err := NewRuleDistiller().Distill(context.Background(), cir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d2.Summary, "The user is dogfooding cxthub.") {
		t.Fatalf("real agent summary lost priority:\n%s", d2.Summary[:200])
	}
}
