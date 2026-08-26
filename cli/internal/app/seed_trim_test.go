package app

import (
	"encoding/json"
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

func TestTrimEventsForSeedKeepsAuthoritativeReplacementPrefix(t *testing.T) {
	prefix := []domain.Event{
		seedMessage("user", "compacted baseline", 0),
		{Kind: domain.EventToolCall, Seq: 1, CallID: "baseline-call", ToolName: "shell", Input: map[string]interface{}{}},
		{Kind: domain.EventCompaction, Seq: 2, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "PINNED-ENC"}},
	}
	cir := domain.CIRDocument{Events: append(append([]domain.Event{}, prefix...),
		seedMessage("user", "old suffix request", 3),
		seedMessage("assistant", strings.Repeat("oversized suffix ", 30000), 4),
		seedMessage("user", "recent request", 5),
		domain.Event{Kind: domain.EventToolResult, Seq: 6, CallID: "baseline-call", Output: "late result"},
		seedMessage("assistant", "recent answer", 7),
	)}
	budget := eventsJSONBytes(prefix) + eventsJSONBytes(cir.Events[5:]) + 128

	got, omitted, kept := trimEventsForSeedKeepingPrefix(cir, len(prefix), budget)
	if !kept {
		t.Fatal("replacement prefix unexpectedly fell back to ordinary tail trimming")
	}
	if eventsJSONBytes(got.Events) > budget {
		t.Fatalf("pinned trim exceeds budget: %d > %d", eventsJSONBytes(got.Events), budget)
	}
	if len(got.Events) < len(prefix)+3 || got.Events[0].Blocks[0].Text != "compacted baseline" || got.Events[2].Locked == nil || got.Events[2].Locked.Blob != "PINNED-ENC" {
		t.Fatalf("authoritative replacement prefix was sliced: %+v", got.Events)
	}
	if got.Events[len(prefix)].Blocks[0].Text != "recent request" {
		t.Fatalf("recent suffix did not start at user boundary: %+v", got.Events[len(prefix):])
	}
	if len(omitted) != 2 {
		t.Fatalf("omitted suffix = %+v", omitted)
	}
	for _, ev := range got.Events {
		if ev.Kind == domain.EventToolResult && ev.CallID == "baseline-call" {
			return
		}
	}
	t.Fatal("tool result paired with a pinned call was removed")
}

func TestTrimEventsForSeedNeverDropsOversizedLockedCompactionState(t *testing.T) {
	locked := strings.Repeat("opaque-state-", 4096)
	prefix := []domain.Event{
		seedMessage("user", strings.Repeat("old semantic baseline ", 100), 0),
		{Kind: domain.EventCompaction, Seq: 1, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: locked}},
	}
	cir := domain.CIRDocument{Events: append(append([]domain.Event{}, prefix...),
		seedMessage("user", "recent request", 2),
		seedMessage("assistant", "recent answer", 3),
		domain.Event{Kind: domain.EventToolCall, Seq: 4, CallID: "recent-call", ToolName: "shell", Input: map[string]interface{}{"cmd": "pwd"}},
		domain.Event{Kind: domain.EventToolResult, Seq: 5, CallID: "recent-call", Output: "/work/project"},
	)}
	budget := eventsJSONBytes(cir.Events[2:]) + 128

	got, omitted, kept := trimEventsForSeedKeepingPrefix(cir, len(prefix), budget)
	if kept {
		t.Fatal("oversized replacement was incorrectly reported as fully pinned")
	}
	if len(omitted) == 0 {
		t.Fatal("oversized replacement did not trim any semantic history")
	}
	var foundLocked, foundRecent, foundAnswer, foundCall, foundResult bool
	semanticBytes := 0
	for _, ev := range got.Events {
		if ev.Kind == domain.EventCompaction && ev.Locked != nil && ev.Locked.Blob == locked {
			foundLocked = true
			continue
		}
		encoded, _ := json.Marshal(ev)
		semanticBytes += len(encoded)
		if ev.Kind == domain.EventMessage && len(ev.Blocks) > 0 && ev.Blocks[0].Text == "recent request" {
			foundRecent = true
		}
		if ev.Kind == domain.EventMessage && len(ev.Blocks) > 0 && ev.Blocks[0].Text == "recent answer" {
			foundAnswer = true
		}
		if ev.Kind == domain.EventToolCall && ev.CallID == "recent-call" {
			foundCall = true
		}
		if ev.Kind == domain.EventToolResult && ev.CallID == "recent-call" {
			foundResult = true
		}
	}
	if !foundLocked {
		t.Fatal("opaque compaction state was sliced by the semantic seed budget")
	}
	if !foundRecent {
		t.Fatal("latest user request was lost while pinning opaque state")
	}
	if !foundAnswer || !foundCall || !foundResult {
		t.Fatalf("recent semantic turn was split while pinning opaque state: answer=%v call=%v result=%v", foundAnswer, foundCall, foundResult)
	}
	if semanticBytes > budget {
		t.Fatalf("semantic replay = %d bytes, want <= %d", semanticBytes, budget)
	}
	replay := asCodexCompactedReplay(got)
	if len(replay.Events) != 1 || replay.Events[0].Kind != domain.EventCompaction || !replay.Events[0].ReplacementComplete {
		t.Fatalf("bounded context was not wrapped as an authoritative native replay: %+v", replay.Events)
	}
	foundLocked = false
	for _, ev := range replay.Events[0].Replacement {
		if ev.Kind == domain.EventCompaction && ev.Locked != nil && ev.Locked.Blob == locked {
			foundLocked = true
		}
	}
	if !foundLocked {
		t.Fatal("native replay wrapper dropped the pinned opaque state")
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

func TestSeedConversationUsesLatestCompactionAndDropsReplayControl(t *testing.T) {
	cir := domain.CIRDocument{Events: []domain.Event{
		seedMessage("user", "archival request before compaction", 0),
		{
			Kind: domain.EventCompaction, Seq: 1,
			ReplacementComplete: true,
			Replacement: []domain.Event{
				seedMessage("user", "[cxt seed] Branch-switch context: main → old\nlegacy seed", 0),
				seedMessage("developer", "runtime instructions", 1),
				seedMessage("user", "<environment_context>\n<cwd>/old</cwd>\n</environment_context>", 2),
				{Kind: domain.EventMessage, Role: "user", Seq: 3, CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "provider compact summary"}}},
				seedMessage("user", "actual request retained", 4),
				seedMessage("assistant", "actual answer retained", 5),
				{Kind: domain.EventMessage, Role: "assistant", Seq: 6, AgentMessage: true, AgentAuthor: "/root/reviewer", AgentRecipient: "/root", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "agent-opaque"}, Blocks: []domain.ContentBlock{{Type: "text", Text: "visible agent result retained"}}},
				{Kind: domain.EventMessage, Role: "assistant", Seq: 7, AgentMessage: true, AgentAuthor: "/root/hidden", AgentRecipient: "/root", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "encrypted-only"}},
				{Kind: domain.EventCompaction, Seq: 8, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}},
			},
		},
		seedMessage("user", "request after compaction retained", 2),
	}}

	got := seedConversationContext(cir)
	if len(got.Events) != 4 {
		t.Fatalf("seed conversation events = %+v", got.Events)
	}
	texts := []string{got.Events[0].Blocks[0].Text, got.Events[1].Blocks[0].Text, got.Events[2].Blocks[0].Text, got.Events[3].Blocks[0].Text}
	want := []string{"actual request retained", "actual answer retained", "visible agent result retained", "request after compaction retained"}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("seed conversation[%d] = %q, want %q", i, texts[i], want[i])
		}
	}
	if got.Events[2].AgentMessage || got.Events[2].AgentAuthor != "" || got.Events[2].AgentRecipient != "" || got.Events[2].Locked != nil {
		t.Fatalf("new branch transplanted provider-local agent state: %+v", got.Events[2])
	}
}
