package codec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// --- Synthetic Fixture (documented format, not actual session content) ---

func TestCodexContentUnionWireShape(t *testing.T) {
	visible, err := json.Marshal(codexContent{Type: "output_text", Text: ""})
	if err != nil {
		t.Fatal(err)
	}
	if string(visible) != `{"text":"","type":"output_text"}` {
		t.Fatalf("empty visible content lost required text: %s", visible)
	}
	encrypted, err := json.Marshal(codexContent{Type: "encrypted_content", EncryptedContent: "ENC"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encrypted) != `{"encrypted_content":"ENC","type":"encrypted_content"}` {
		t.Fatalf("encrypted content union is malformed: %s", encrypted)
	}
}

func TestCodexCompactedReplayEmitsVisibleTranscriptCompanions(t *testing.T) {
	doc := domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion:      domain.CIRVersionV2,
			SourceProvider:  domain.ProviderCodex,
			SessionOriginID: "roll-display",
			CapturedAt:      "2026-08-26T00:00:00Z",
		},
		Events: []domain.Event{{
			Kind: domain.EventCompaction, Ts: "2026-08-26T00:01:00Z",
			ReplacementComplete: true,
			Replacement: []domain.Event{
				{Kind: domain.EventMessage, Role: "user", Ts: "2026-08-26T00:00:01Z", Blocks: []domain.ContentBlock{{Type: "text", Text: "visible request"}}},
				{Kind: domain.EventMessage, Role: "assistant", Ts: "2026-08-26T00:00:02Z", Blocks: []domain.ContentBlock{{Type: "text", Text: "visible reply"}}},
				{Kind: domain.EventMessage, Role: "system", Ts: "2026-08-26T00:00:02Z", Blocks: []domain.ContentBlock{{Type: "text", Text: "hidden system runtime"}}},
				{Kind: domain.EventMessage, Role: "developer", Ts: "2026-08-26T00:00:02Z", Blocks: []domain.ContentBlock{{Type: "text", Text: "hidden developer runtime"}}},
				{Kind: domain.EventMessage, Role: "assistant", AgentMessage: true, AgentAuthor: "/root/reviewer", AgentRecipient: "/root", Ts: "2026-08-26T00:00:03Z", Blocks: []domain.ContentBlock{{Type: "text", Text: "provider-internal agent message"}}, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "ENC-AGENT"}},
			},
		}},
	}

	wire, err := NewCodexCodec().Encode(context.Background(), doc, domain.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	var display []codexEventMsg
	for _, rawLine := range strings.Split(strings.TrimSpace(string(wire)), "\n") {
		var line codexLine
		if err := json.Unmarshal([]byte(rawLine), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type != "event_msg" {
			continue
		}
		var event codexEventMsg
		if err := json.Unmarshal(line.Payload, &event); err != nil {
			t.Fatal(err)
		}
		display = append(display, event)
	}
	if len(display) != 2 || display[0].Type != "user_message" || display[0].Message != "visible request" || display[1].Type != "agent_message" || display[1].Message != "visible reply" {
		t.Fatalf("compacted transcript companions = %+v\n%s", display, wire)
	}
	if strings.Contains(string(wire), `"message":"provider-internal agent message"`) {
		t.Fatalf("provider-internal agent item leaked into visible transcript:\n%s", wire)
	}
}

func TestCodexCompleteReplacementNeverSilentlyDropsUnsupportedEvent(t *testing.T) {
	doc := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex},
		Events: []domain.Event{{
			Kind: domain.EventCompaction, ReplacementComplete: true,
			Replacement: []domain.Event{{Kind: domain.EventTurn, Role: "user"}},
		}},
	}
	if _, err := NewCodexCodec().Encode(context.Background(), doc, domain.ProviderCodex); err == nil {
		t.Fatal("unsupported replacement event was silently omitted")
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestToolMappingBidirectional(t *testing.T) {
	fwd := map[string]string{
		"Bash":                      toolShell,
		"Edit":                      toolApplyPatch,
		"MultiEdit":                 toolApplyPatch,
		"Write":                     toolApplyPatch,
		"Read":                      toolReadFile,
		"LS":                        toolListDir,
		"Grep":                      toolGrep,
		"WebSearch":                 toolWebSearch,
		"TodoWrite":                 toolUpdatePlan,
		"shell":                     toolShell,
		"exec_command":              toolShell,
		"unified_exec":              toolShell,
		"write_stdin":               toolShell,
		"apply_patch":               toolApplyPatch,
		"write_file":                toolApplyPatch,
		"read_file":                 toolReadFile,
		"view_image":                toolReadFile,
		"NotebookEdit":              toolApplyPatch,
		"mcp__github__create_issue": "mcp:github:create_issue",
		"FancyUnknownTool":          "unknown:FancyUnknownTool",
	}
	for in, want := range fwd {
		if got := canonicalToolName(in); got != want {
			t.Errorf("canonicalToolName(%q)=%q want %q", in, got, want)
		}
	}
	// Reverse (for cross-encoding)
	if got := providerToolName(domain.ProviderClaude, toolShell); got != "Bash" {
		t.Errorf("reverse claude shell=%q want Bash", got)
	}
	if got := providerToolName(domain.ProviderCodex, toolReadFile); got != "shell" {
		t.Errorf("reverse codex read_file=%q want shell (downgrade)", got)
	}
	if got := providerToolName(domain.ProviderClaude, "mcp:github:create_issue"); got != "mcp__github__create_issue" {
		t.Errorf("reverse mcp=%q", got)
	}
	if got := providerToolName(domain.ProviderClaude, "unknown:FancyUnknownTool"); got != "FancyUnknownTool" {
		t.Errorf("reverse unknown=%q want FancyUnknownTool", got)
	}
}

// Schema compliance: nil output→"", reasoning cross_replayable=false, message includes role.
func TestSchemaConformance(t *testing.T) {
	tr := domain.Event{Kind: domain.EventToolResult, CallID: "c1", Output: nil}
	b := mustJSON(t, tr)
	if strings.Contains(b, "null") || !strings.Contains(b, `"output":""`) {
		t.Errorf("tool_result nil output must serialize as empty string, got %s", b)
	}
	rs := domain.Event{Kind: domain.EventReasoning, CrossReplayable: true,
		Locked: &domain.LockedBlob{Provider: domain.ProviderClaude, Scheme: "signature", Blob: "x"}}
	b = mustJSON(t, rs)
	if !strings.Contains(b, `"cross_replayable":false`) {
		t.Errorf("reasoning cross_replayable must be const false, got %s", b)
	}
	msg := domain.Event{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "hi"}}}
	b = mustJSON(t, msg)
	if !strings.Contains(b, `"role":"user"`) {
		t.Errorf("message must include role, got %s", b)
	}
}
