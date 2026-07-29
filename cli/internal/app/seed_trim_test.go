package app

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func seedMessage(role, text string, seq int) domain.Event {
	return domain.Event{
		Kind: domain.EventMessage,
		Role: role,
		Seq:  seq,
		Blocks: []domain.ContentBlock{{
			Type: "text",
			Text: text,
		}},
	}
}

func TestTrimEventsForSeedStartsAtNextUserBoundary(t *testing.T) {
	cir := domain.CIRDocument{Events: []domain.Event{
		seedMessage("user", "old prompt", 0),
		seedMessage("assistant", strings.Repeat("old answer ", 80), 1),
		seedMessage("user", "recent prompt", 2),
		seedMessage("assistant", "recent answer", 3),
	}}
	budget := eventsJSONBytes(cir.Events[2:])

	got, omitted := trimEventsForSeed(cir, budget)
	if len(got.Events) != 2 || got.Events[0].Blocks[0].Text != "recent prompt" {
		t.Fatalf("trim did not start at the next user boundary: %+v", got.Events)
	}
	if len(omitted) != 2 {
		t.Fatalf("omitted count: got %d want 2", len(omitted))
	}
}

func TestTrimEventsForSeedAnchorsLatestUserInOversizedTurn(t *testing.T) {
	cir := domain.CIRDocument{Events: []domain.Event{
		seedMessage("user", "old prompt", 0),
		seedMessage("assistant", strings.Repeat("old answer ", 80), 1),
		seedMessage("user", "current sync request", 2),
		{
			Kind: domain.EventToolCall, Seq: 3, CallID: "omitted-call", ToolName: "exec",
			Input: map[string]interface{}{"payload": strings.Repeat("large input ", 80)},
		},
		{Kind: domain.EventToolResult, Seq: 4, CallID: "omitted-call", Output: "orphaned result"},
		{Kind: domain.EventReasoning, Seq: 5, RedactedSummary: "continue diagnosis"},
		{Kind: domain.EventToolCall, Seq: 6, CallID: "kept-call", ToolName: "exec"},
		{Kind: domain.EventToolResult, Seq: 7, CallID: "kept-call", Output: "verified"},
		seedMessage("assistant", "latest progress", 8),
	}}
	budget := eventsJSONBytes([]domain.Event{cir.Events[2], cir.Events[4], cir.Events[5], cir.Events[6], cir.Events[7], cir.Events[8]})

	got, omitted := trimEventsForSeed(cir, budget)
	if len(got.Events) == 0 || got.Events[0].Blocks[0].Text != "current sync request" {
		t.Fatalf("latest user request was not anchored: %+v", got.Events)
	}
	if eventsJSONBytes(got.Events) > budget {
		t.Fatalf("trimmed events exceed budget: got %d want <= %d", eventsJSONBytes(got.Events), budget)
	}

	calls := map[string]bool{}
	for _, ev := range got.Events {
		if ev.Kind == domain.EventToolCall {
			calls[ev.CallID] = true
		}
	}
	for _, ev := range got.Events {
		if ev.Kind == domain.EventToolResult && !calls[ev.CallID] {
			t.Fatalf("unmatched tool result retained: %s", ev.CallID)
		}
	}
	if calls["omitted-call"] {
		t.Fatal("oversized old tool call unexpectedly retained")
	}
	if !calls["kept-call"] {
		t.Fatal("recent complete tool pair was not retained")
	}
	if len(got.Events)+len(omitted) != len(cir.Events) {
		t.Fatalf("selected/omitted partition lost events: selected=%d omitted=%d total=%d", len(got.Events), len(omitted), len(cir.Events))
	}
}

func TestRenderSeedDigestIsBoundedAndKeepsRecentState(t *testing.T) {
	const budget = 8 << 10
	digest := domain.MemoryDigest{
		Summary: strings.Repeat("résumé history ", 5000) + "\nLATEST DECISION: continue from the public repository.",
		KeyFacts: []string{
			"the restored session must use the target working directory",
		},
		OpenTasks: []string{
			"verify the newest pending snapshot before ending migration",
		},
	}

	got := renderSeedDigest(digest, 1234, budget)
	if len(got) > budget {
		t.Fatalf("digest exceeds budget: got %d want <= %d", len(got), budget)
	}
	if !utf8.ValidString(got) {
		t.Fatal("digest truncation produced invalid UTF-8")
	}
	for _, want := range []string{
		"LATEST DECISION",
		"target working directory",
		"verify the newest pending snapshot",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bounded digest lost %q", want)
		}
	}
	if !strings.Contains(got, "earlier summary omitted") {
		t.Fatal("large summary was not visibly truncated")
	}
}
