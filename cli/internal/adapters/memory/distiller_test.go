package memory

import (
	"context"
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
