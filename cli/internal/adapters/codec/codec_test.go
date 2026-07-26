package codec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// --- Synthetic Fixture (documented format, not actual session content) ---

const claudeFixture = `{"type":"user","cwd":"/work/proj","sessionId":"sess-1","gitBranch":"main","version":"2.1.195","timestamp":"2026-06-30T00:00:00Z","message":{"role":"user","content":"analyze this repo"}}
{"type":"assistant","cwd":"/work/proj","sessionId":"sess-1","gitBranch":"main","version":"2.1.195","timestamp":"2026-06-30T00:00:01Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"let me look","signature":"SIG-ABC"},{"type":"text","text":"I'll inspect the files."},{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":850,"output_tokens":42}}}
{"type":"queue-operation","operation":"x","timestamp":"2026-06-30T00:00:01Z"}
{"type":"user","cwd":"/work/proj","sessionId":"sess-1","gitBranch":"main","version":"2.1.195","timestamp":"2026-06-30T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"file1.go\nfile2.go"}]}}`

const codexFixture = `{"timestamp":"2026-06-30T01:00:00Z","type":"session_meta","payload":{"id":"roll-1","timestamp":"2026-06-30T01:00:00Z","cwd":"/work/proj","cli_version":"0.142.2","model_provider":"openai","model":"gpt-5-codex"}}
{"timestamp":"2026-06-30T01:00:00Z","type":"turn_context","payload":{"turn_id":"t-1","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-06-30T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor the parser"}]}}
{"timestamp":"2026-06-30T01:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"planning the refactor"}],"encrypted_content":"ENC-XYZ"}}
{"timestamp":"2026-06-30T01:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Editing now."}]}}
{"timestamp":"2026-06-30T01:00:04Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"go build\"}","call_id":"fc-1"}}
{"timestamp":"2026-06-30T01:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"fc-1","output":"ok"}}
{"timestamp":"2026-06-30T01:00:06Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"ct-1","name":"apply_patch","input":"*** Begin Patch\n+hi\n*** End Patch"}}
{"timestamp":"2026-06-30T01:00:07Z","type":"event_msg","payload":{"note":"ui only"}}
{"timestamp":"2026-06-30T01:00:08Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50000,"cached_input_tokens":40000,"output_tokens":321,"total_tokens":50321},"last_token_usage":{"input_tokens":42000,"cached_input_tokens":40000,"output_tokens":100,"total_tokens":42100},"model_context_window":258400},"rate_limits":{"limit_id":"codex"}}}`

// TestClaudeMultiModel fixes model attribution during session transitions:
// source_models accumulates in order (no duplicates), source_model is the last used (last-wins —
// previous first-wins caused model switches in the latter part of the session).
func TestClaudeMultiModel(t *testing.T) {
	fixture := `{"type":"assistant","sessionId":"s","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a"}]}}
{"type":"assistant","sessionId":"s","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"b"}]}}
{"type":"assistant","sessionId":"s","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"c"}]}}`
	cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cir.Envelope.SourceModel != "claude-fable-5" {
		t.Fatalf("source_model last-wins failure: %q", cir.Envelope.SourceModel)
	}
	if got := strings.Join(cir.Envelope.SourceModels, ","); got != "claude-opus-4-8,claude-fable-5" {
		t.Fatalf("source_models accumulation failure: %q", got)
	}
}

// TestClaudeCompactionCount fixes the behavior of counting context compaction boundaries (type=="system"&&subtype=="compact_boundary"):
// sessionId should remain unchanged (continuing the same file) and the compaction count should be accurate,
// and the marker itself should not be included as an event (the only signal of a graph compaction marker (◈)).
func TestClaudeCompactionCount(t *testing.T) {
	fixture := `{"type":"user","sessionId":"s","timestamp":"2026-07-01T00:00:00Z","message":{"role":"user","content":"go"}}
{"type":"system","subtype":"compact_boundary","sessionId":"s","timestamp":"2026-07-01T00:01:00Z","compactMetadata":{"trigger":"auto","preTokens":1000000,"postTokens":11000}}
{"type":"assistant","sessionId":"s","timestamp":"2026-07-01T00:02:00Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"ok"}]}}
{"type":"system","subtype":"compact_boundary","sessionId":"s","timestamp":"2026-07-01T00:03:00Z","compactMetadata":{"trigger":"auto"}}`
	cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cir.Envelope.CompactionCount != 2 {
		t.Fatalf("compaction_count expected 2, got %d", cir.Envelope.CompactionCount)
	}
	if cir.Envelope.SessionOriginID != "s" {
		t.Fatalf("compaction should not change sessionId: %q", cir.Envelope.SessionOriginID)
	}
	if got := len(cir.Events); got != 2 {
		t.Fatalf("compact_boundary marker is not an event — expected 2(user,assistant), got %d", got)
	}
}

// TestCodexCompactionCount ensures that the codex compaction boundary (type=="compacted" — empirically verified rollout format) is fixed (matches claude compact_boundary and parity). Markers are not embedded as events.
func TestCodexCompactionCount(t *testing.T) {
	fixture := `{"timestamp":"2026-07-01T00:00:00Z","type":"session_meta","payload":{"id":"roll-c","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}
{"timestamp":"2026-07-01T00:01:00Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}}
{"timestamp":"2026-07-01T00:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}
{"timestamp":"2026-07-01T00:03:00Z","type":"compacted","payload":{"message":"","replacement_history":[]}}`
	cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cir.Envelope.CompactionCount != 2 {
		t.Fatalf("compaction_count expected 2, got %d", cir.Envelope.CompactionCount)
	}
	if cir.Envelope.SessionOriginID != "roll-c" {
		t.Fatalf("compaction should not change session id: %q", cir.Envelope.SessionOriginID)
	}
	if got := len(cir.Events); got != 2 {
		t.Fatalf("compacted marker is not an event — expected 2(user,assistant), got %d", got)
	}
}

// TestCodexMultiModel ensures that codex session model switching attribution is fixed (matches claude meaning):
// records session_meta model + turn_context model as last-wins representative value + cumulative occurrence.
func TestCodexMultiModel(t *testing.T) {
	fixture := `{"timestamp":"2026-06-30T01:00:00Z","type":"session_meta","payload":{"id":"roll-2","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-06-30T01:00:01Z","type":"turn_context","payload":{"turn_id":"t-1","model":"gpt-5.5"}}
{"timestamp":"2026-06-30T01:00:02Z","type":"turn_context","payload":{"turn_id":"t-2","model":"gpt-5.5"}}`
	cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cir.Envelope.SourceModel != "gpt-5.5" {
		t.Fatalf("source_model last-wins failure: %q", cir.Envelope.SourceModel)
	}
	if got := strings.Join(cir.Envelope.SourceModels, ","); got != "gpt-5-codex,gpt-5.5" {
		t.Fatalf("source_models cumulative failure: %q", got)
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

// roundtrip: validates that decode → encode(same provider) → decode preserves CIR (meaning equality) in a normal provider-native fixture.
// intentional normalization of target schema incompatible outputs is fixed separately in provider-specific tests below.
func TestClaudeRoundtrip(t *testing.T) {
	ctx := context.Background()
	cc := NewClaudeCodec()
	cir1, err := cc.Decode(ctx, []byte(claudeFixture))
	if err != nil {
		t.Fatalf("decode1: %v", err)
	}
	if got := len(cir1.Events); got != 5 {
		t.Fatalf("expected 5 events (queue-operation dropped), got %d", got)
	}
	if cir1.Envelope.GitBranch != "main" || cir1.Envelope.SessionOriginID != "sess-1" || cir1.Envelope.SourceModel != "claude-opus-4-8" {
		t.Fatalf("envelope not extracted: %+v", cir1.Envelope)
	}
	// Token stats: context = input+cache_creation+cache_read(100+50+850), output = total.
	if cir1.Envelope.ContextTokens != 1000 || cir1.Envelope.OutputTokens != 42 {
		t.Fatalf("token stats not extracted: ctx=%d out=%d", cir1.Envelope.ContextTokens, cir1.Envelope.OutputTokens)
	}
	b, err := cc.Encode(ctx, cir1, domain.ProviderClaude)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	cir2, err := cc.Decode(ctx, b)
	if err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if mustJSON(t, cir1) != mustJSON(t, cir2) {
		t.Errorf("claude roundtrip not lossless\ncir1=%s\ncir2=%s", mustJSON(t, cir1), mustJSON(t, cir2))
	}
	// Same provider full: signature preserved.
	if !strings.Contains(string(b), "SIG-ABC") {
		t.Errorf("claude full encode should preserve signature, got: %s", b)
	}
}

func TestCodexRoundtrip(t *testing.T) {
	ctx := context.Background()
	cx := NewCodexCodec()
	cir1, err := cx.Decode(ctx, []byte(codexFixture))
	if err != nil {
		t.Fatalf("decode1: %v", err)
	}
	if got := len(cir1.Events); got != 6 {
		t.Fatalf("expected 6 events (event_msg/turn_context dropped), got %d", got)
	}
	if cir1.Envelope.SessionOriginID != "roll-1" || cir1.Envelope.SourceModel != "gpt-5-codex" {
		t.Fatalf("envelope not extracted: %+v", cir1.Envelope)
	}
	// Token stats(token_count event_msg): context = last_token_usage.input(last observed),
	// output = total_token_usage.output(session cumulative last-wins).
	if cir1.Envelope.ContextTokens != 42000 || cir1.Envelope.OutputTokens != 321 {
		t.Fatalf("token stats not extracted: ctx=%d out=%d", cir1.Envelope.ContextTokens, cir1.Envelope.OutputTokens)
	}
	// reasoning.summary → redacted_summary plaintext preserved.
	foundSummary := false
	for _, ev := range cir1.Events {
		if ev.Kind == domain.EventReasoning && ev.RedactedSummary == "planning the refactor" {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("reasoning summary not extracted into redacted_summary")
	}
	b, err := cx.Encode(ctx, cir1, domain.ProviderCodex)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	cir2, err := cx.Decode(ctx, b)
	if err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if mustJSON(t, cir1) != mustJSON(t, cir2) {
		t.Errorf("codex roundtrip not lossless\ncir1=%s\ncir2=%s", mustJSON(t, cir1), mustJSON(t, cir2))
	}
	if !strings.Contains(string(b), "ENC-XYZ") {
		t.Errorf("codex full encode should preserve encrypted_content")
	}
}

// Cross-encode: claude→CIR→codex. Text/toolcall preserved, claude signature anonymized.
func TestCrossClaudeToCodex(t *testing.T) {
	ctx := context.Background()
	cir, err := NewClaudeCodec().Decode(ctx, []byte(claudeFixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := NewCodexCodec().Encode(ctx, cir, domain.ProviderCodex)
	if err != nil {
		t.Fatalf("cross encode: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "SIG-ABC") {
		t.Errorf("cross-encode must NOT leak claude signature into codex output:\n%s", s)
	}
	if strings.Contains(s, "encrypted_content") {
		t.Errorf("cross-encode must not fabricate codex encrypted_content from a claude-sourced reasoning")
	}
	if !strings.Contains(s, "I'll inspect the files.") {
		t.Errorf("cross-encode should preserve assistant text")
	}
	// Toolcall mapped to codex vocabulary (Bash→shell).
	if !strings.Contains(s, "\"name\":\"shell\"") {
		t.Errorf("cross-encode should remap tool name to codex (shell), got:\n%s", s)
	}
}

// Cross-encode: codex→CIR→claude. codex encrypted_content anonymized.
func TestCrossCodexToClaude(t *testing.T) {
	ctx := context.Background()
	cir, err := NewCodexCodec().Decode(ctx, []byte(codexFixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := NewClaudeCodec().Encode(ctx, cir, domain.ProviderClaude)
	if err != nil {
		t.Fatalf("cross encode: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "ENC-XYZ") {
		t.Errorf("cross-encode must NOT leak codex encrypted_content into claude output:\n%s", s)
	}
	if strings.Contains(s, "\"signature\"") {
		t.Errorf("cross-encode must not fabricate a claude signature")
	}
	if !strings.Contains(s, "Editing now.") {
		t.Errorf("cross-encode should preserve assistant text")
	}
	// codex reasoning summary promoted to text in reconstruction (gap of 6 resolved).
	if !strings.Contains(s, "planning the refactor") {
		t.Errorf("cross-encode should preserve codex reasoning summary as degraded text:\n%s", s)
	}
}

// TestCrossCodexToolResultContentUsesClaudeSchema ensures that the Codex input_text and the past
// polluted output_text block array are not directly passed as Claude tool_result.content. CIR
// preserves the original array but forces text fallback at the Claude cross-encoding boundary.
// Claude does not allow input_text at this position, causing all requests after resume to fail with 400.
func TestCrossCodexToolResultContentUsesClaudeSchema(t *testing.T) {
	fixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-tool-result","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"first line"},{"type":"output_text","text":"second line"}]}}`

	cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("codex decode: %v", err)
	}
	wantCodexBlocks := []interface{}{
		map[string]interface{}{"type": "input_text", "text": "first line"},
		map[string]interface{}{"type": "output_text", "text": "second line"},
	}
	if len(cir.Events) != 1 || mustJSON(t, cir.Events[0].Output) != mustJSON(t, wantCodexBlocks) {
		t.Fatalf("CIR tool_result must preserve provider block arrays, got %#v", cir.Events)
	}
	// Older CIR versions may still contain arrays. A new decoder must be fixed to avoid the same 400
	// error when materializing the snapshot with Claude, so the encode boundary must also be independently defended.
	out, err := NewClaudeCodec().Encode(context.Background(), cir, domain.ProviderClaude)
	if err != nil {
		t.Fatalf("claude encode: %v", err)
	}
	got := emittedClaudeToolResultContent(t, out)
	if got != "first line\n\nsecond line" {
		t.Fatalf("Claude tool_result content must be joined text, got %#v\n%s", got, out)
	}
	if strings.Contains(string(out), `"type":"input_text"`) || strings.Contains(string(out), `"type":"output_text"`) {
		t.Fatalf("Codex-only content tags leaked into Claude tool_result:\n%s", out)
	}
}

// TestCrossClaudeToolResultContentUsesCodexSchema ensures that the Claude native
// text/image/tool_reference array is not directly passed as Codex function_call_output.output.
// The structured array allowed by the Responses API at this position is limited to input_text/input_image/input_file.
// Therefore, cross-provider encoding must convert it to provider-neutral plain text.
func TestCrossClaudeToolResultContentUsesCodexSchema(t *testing.T) {
	fixture := `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"screenshot:"},{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"UklGRg=="}},{"type":"tool_reference","tool_name":"Monitor"}]}]}}`

	cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("claude decode: %v", err)
	}
	out, err := NewCodexCodec().Encode(context.Background(), cir, domain.ProviderCodex)
	if err != nil {
		t.Fatalf("codex encode: %v", err)
	}
	got := emittedCodexToolResultOutput(t, out)
	text, ok := got.(string)
	if !ok {
		t.Fatalf("Claude-only blocks must degrade to Codex text, got %#v\n%s", got, out)
	}
	if !strings.Contains(text, "screenshot:") || !strings.Contains(text, `"type":"image"`) || !strings.Contains(text, `"type":"tool_reference"`) {
		t.Fatalf("degraded Codex text must retain every Claude block, got %q", text)
	}
}

func TestClaudeToolResultNormalization(t *testing.T) {
	t.Run("native multimodal blocks roundtrip losslessly", func(t *testing.T) {
		fixture := `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"screenshot:"},{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"UklGRg=="}},{"type":"tool_reference","tool_name":"Monitor"}]}]}}`
		cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := []interface{}{
			map[string]interface{}{"type": "text", "text": "screenshot:"},
			map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/webp", "data": "UklGRg=="}},
			map[string]interface{}{"type": "tool_reference", "tool_name": "Monitor"},
		}
		if len(cir.Events) != 1 || mustJSON(t, cir.Events[0].Output) != mustJSON(t, want) {
			t.Fatalf("Claude native blocks must survive CIR decode, got %#v", cir.Events)
		}
		out, err := NewClaudeCodec().Encode(context.Background(), cir, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, out); mustJSON(t, got) != mustJSON(t, want) {
			t.Fatalf("Claude native blocks must roundtrip losslessly, got %#v\n%s", got, out)
		}
	})

	t.Run("contaminated native session still rejects Codex tags", func(t *testing.T) {
		// Recapturing a Claude file created by an older cxt may result in the envelope source being Claude,
		// but the internal array may still contain Codex input_text. Trusting the array as a full path will
		// cause the original API 400 error to reoccur.
		fixture := `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"input_text","text":"legacy result"}]}]}}`
		cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		out, err := NewClaudeCodec().Encode(context.Background(), cir, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, out); got != "legacy result" {
			t.Fatalf("Codex-only tag in Claude source must degrade to text, got %#v\n%s", got, out)
		}
		if strings.Contains(string(out), `"type":"input_text"`) {
			t.Fatalf("Codex-only tag leaked through Claude full encode:\n%s", out)
		}
	})

	t.Run("empty block array encodes as text", func(t *testing.T) {
		fixture := `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[]}]}}`
		cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(cir.Events) != 1 || mustJSON(t, cir.Events[0].Output) != `[]` {
			t.Fatalf("CIR must preserve the empty source array, got %#v", cir.Events)
		}
		out, err := NewClaudeCodec().Encode(context.Background(), cir, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, out); got != "" {
			t.Fatalf("empty Claude block array must degrade to empty text, got %#v\n%s", got, out)
		}
	})

	t.Run("object output encodes as Claude text", func(t *testing.T) {
		doc := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "s"},
			Events: []domain.Event{{
				Kind: domain.EventToolResult, CallID: "call-1",
				Output: map[string]interface{}{"ok": true, "count": 2},
			}},
		}
		out, err := NewClaudeCodec().Encode(context.Background(), doc, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, out); got != `{"count":2,"ok":true}` {
			t.Fatalf("Claude tool_result object must be JSON text, got %#v\n%s", got, out)
		}
	})
}

func TestCodexToolResultNormalization(t *testing.T) {
	t.Run("native multimodal blocks roundtrip losslessly", func(t *testing.T) {
		fixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-native","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"screenshot:"},{"type":"input_image","image_url":"data:image/webp;base64,UklGRg=="},{"type":"input_file","file_id":"file-1"}]}}`
		cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := []interface{}{
			map[string]interface{}{"type": "input_text", "text": "screenshot:"},
			map[string]interface{}{"type": "input_image", "image_url": "data:image/webp;base64,UklGRg=="},
			map[string]interface{}{"type": "input_file", "file_id": "file-1"},
		}
		if len(cir.Events) != 1 || mustJSON(t, cir.Events[0].Output) != mustJSON(t, want) {
			t.Fatalf("Codex native blocks must survive CIR decode, got %#v", cir.Events)
		}
		out, err := NewCodexCodec().Encode(context.Background(), cir, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, out); mustJSON(t, got) != mustJSON(t, want) {
			t.Fatalf("Codex native blocks must roundtrip losslessly, got %#v\n%s", got, out)
		}
	})

	t.Run("contaminated native session rejects Claude tags", func(t *testing.T) {
		fixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-contaminated","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"text","text":"legacy result"},{"type":"tool_reference","tool_name":"Monitor"}]}}`
		cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		out, err := NewCodexCodec().Encode(context.Background(), cir, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got := emittedCodexToolResultOutput(t, out)
		if _, ok := got.(string); !ok {
			t.Fatalf("Claude-only tags in Codex source must degrade to text, got %#v\n%s", got, out)
		}
	})

	t.Run("output_text is not a native function output block", func(t *testing.T) {
		fixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-output-text","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"output_text","text":"legacy result"}]}}`
		cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		out, err := NewCodexCodec().Encode(context.Background(), cir, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, out); got != "legacy result" {
			t.Fatalf("output_text must degrade to Codex string output, got %#v\n%s", got, out)
		}
	})

	t.Run("empty block array encodes as text", func(t *testing.T) {
		fixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-empty","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[]}}`
		cir, err := NewCodexCodec().Decode(context.Background(), []byte(fixture))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(cir.Events) != 1 || mustJSON(t, cir.Events[0].Output) != `[]` {
			t.Fatalf("CIR must preserve the empty source array, got %#v", cir.Events)
		}
		out, err := NewCodexCodec().Encode(context.Background(), cir, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, out); got != "" {
			t.Fatalf("empty Codex block array must degrade to empty text, got %#v\n%s", got, out)
		}
	})

	t.Run("object output encodes as Codex text", func(t *testing.T) {
		doc := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "s"},
			Events: []domain.Event{{
				Kind: domain.EventToolResult, CallID: "call-1",
				Output: map[string]interface{}{"ok": true, "count": 2},
			}},
		}
		out, err := NewCodexCodec().Encode(context.Background(), doc, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, out); got != `{"count":2,"ok":true}` {
			t.Fatalf("Codex tool result object must be JSON text, got %#v\n%s", got, out)
		}
	})
}

func TestToolResultNilNormalization(t *testing.T) {
	t.Run("decode null as empty text", func(t *testing.T) {
		claudeFixture := `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":null}]}}`
		claudeCIR, err := NewClaudeCodec().Decode(context.Background(), []byte(claudeFixture))
		if err != nil {
			t.Fatalf("Claude decode: %v", err)
		}
		if len(claudeCIR.Events) != 1 || claudeCIR.Events[0].Output != "" {
			t.Fatalf("Claude null output must normalize to empty text, got %#v", claudeCIR.Events)
		}

		codexFixture := `{"timestamp":"2026-07-22T00:00:00Z","type":"session_meta","payload":{"id":"roll-null","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-07-22T00:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":null}}`
		codexCIR, err := NewCodexCodec().Decode(context.Background(), []byte(codexFixture))
		if err != nil {
			t.Fatalf("Codex decode: %v", err)
		}
		if len(codexCIR.Events) != 1 || codexCIR.Events[0].Output != "" {
			t.Fatalf("Codex null output must normalize to empty text, got %#v", codexCIR.Events)
		}
	})

	t.Run("same-provider encode nil as empty text", func(t *testing.T) {
		claudeDoc := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "s"},
			Events:   []domain.Event{{Kind: domain.EventToolResult, CallID: "call-1", Output: nil}},
		}
		claudeOut, err := NewClaudeCodec().Encode(context.Background(), claudeDoc, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("Claude encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, claudeOut); got != "" {
			t.Fatalf("Claude nil output must encode as empty text, got %#v", got)
		}

		codexDoc := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "s"},
			Events:   []domain.Event{{Kind: domain.EventToolResult, CallID: "call-1", Output: nil}},
		}
		codexOut, err := NewCodexCodec().Encode(context.Background(), codexDoc, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("Codex encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, codexOut); got != "" {
			t.Fatalf("Codex nil output must encode as empty text, got %#v", got)
		}
	})

	t.Run("cross-provider encode nil as empty text", func(t *testing.T) {
		fromCodex := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "s"},
			Events:   []domain.Event{{Kind: domain.EventToolResult, CallID: "call-1", Output: nil}},
		}
		claudeOut, err := NewClaudeCodec().Encode(context.Background(), fromCodex, domain.ProviderClaude)
		if err != nil {
			t.Fatalf("Codex to Claude encode: %v", err)
		}
		if got := emittedClaudeToolResultContent(t, claudeOut); got != "" {
			t.Fatalf("cross-provider nil output must encode as Claude empty text, got %#v", got)
		}

		fromClaude := domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: "s"},
			Events:   []domain.Event{{Kind: domain.EventToolResult, CallID: "call-1", Output: nil}},
		}
		codexOut, err := NewCodexCodec().Encode(context.Background(), fromClaude, domain.ProviderCodex)
		if err != nil {
			t.Fatalf("Claude to Codex encode: %v", err)
		}
		if got := emittedCodexToolResultOutput(t, codexOut); got != "" {
			t.Fatalf("cross-provider nil output must encode as Codex empty text, got %#v", got)
		}
	})
}

func emittedClaudeToolResultContent(t *testing.T, out []byte) interface{} {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var rec claudeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode emitted Claude record: %v", err)
		}
		if rec.Message == nil {
			continue
		}
		blocks, err := parseClaudeContent(rec.Message.Content)
		if err != nil {
			t.Fatalf("decode emitted Claude content: %v", err)
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			var got interface{}
			if err := json.Unmarshal(block.Content, &got); err != nil {
				t.Fatalf("decode tool_result content: %v", err)
			}
			return got
		}
	}
	t.Fatal("emitted Claude session has no tool_result")
	return nil
}

func emittedCodexToolResultOutput(t *testing.T, out []byte) interface{} {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var rec codexLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode emitted Codex record: %v", err)
		}
		if rec.Type != "response_item" {
			continue
		}
		var item codexResponseItem
		if err := json.Unmarshal(rec.Payload, &item); err != nil {
			t.Fatalf("decode emitted Codex response item: %v", err)
		}
		if item.Type != "function_call_output" {
			continue
		}
		var got interface{}
		if err := json.Unmarshal(item.Output, &got); err != nil {
			t.Fatalf("decode Codex function_call_output: %v", err)
		}
		return got
	}
	t.Fatal("emitted Codex session has no function_call_output")
	return nil
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

// CanonicalBytes/Serialization determinism: same input produces the same bytes on repeat.
func TestSerializationDeterministic(t *testing.T) {
	ctx := context.Background()
	cir, err := NewCodexCodec().Decode(ctx, []byte(codexFixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	first := mustJSON(t, normalizeCIR(cir))
	for i := 0; i < 20; i++ {
		if got := mustJSON(t, normalizeCIR(cir)); got != first {
			t.Fatalf("serialization not deterministic at iter %d", i)
		}
	}
}

// TestNoteModelSkipsSynthetic ensures that "<synthetic>" (a placeholder — not a real model — created by the Claude Code harness)
// does not appear in the representative (SourceModel) or accumulated (SourceModels). If not filtered,
// the viewer model display will show "claude-… + <synthetic> + …".
func TestNoteModelSkipsSynthetic(t *testing.T) {
	fixture := `{"type":"assistant","sessionId":"s","timestamp":"2026-07-01T00:00:00Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"a"}]}}
{"type":"assistant","sessionId":"s","timestamp":"2026-07-01T00:01:00Z","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}
{"type":"assistant","sessionId":"s","timestamp":"2026-07-01T00:02:00Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"b"}]}}`
	cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cir.Envelope.SourceModel != "claude-opus-4-8" {
		t.Fatalf("Primary model should be the last model (last-wins): %q", cir.Envelope.SourceModel)
	}
	for _, m := range cir.Envelope.SourceModels {
		if m == "<synthetic>" {
			t.Fatalf("SourceModels should include synthetic placeholder: %v", cir.Envelope.SourceModels)
		}
	}
	if len(cir.Envelope.SourceModels) != 2 {
		t.Fatalf("Expected 2 source models, got %v", cir.Envelope.SourceModels)
	}
}

// TestClaudeCompactSummaryFlag ensures that the agent compact summary (isCompactSummary user record) is captured and preserved as a CompactSummary event during encode round-trip — the primary capture path for memorized data directly written by the agent.
func TestClaudeCompactSummaryFlag(t *testing.T) {
	fixture := `{"type":"user","sessionId":"s","timestamp":"2026-07-01T00:00:00Z","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued... summary body"}}
{"type":"user","sessionId":"s","timestamp":"2026-07-01T00:01:00Z","message":{"role":"user","content":"ordinary prompt"}}`
	cir, err := NewClaudeCodec().Decode(context.Background(), []byte(fixture))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cir.Events) != 2 || !cir.Events[0].CompactSummary || cir.Events[1].CompactSummary {
		t.Fatalf("CompactSummary capture failed: %+v", cir.Events)
	}
	raw, err := NewClaudeCodec().Encode(context.Background(), cir, "claude")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	cir2, err := NewClaudeCodec().Decode(context.Background(), raw)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !cir2.Events[0].CompactSummary {
		t.Fatal("isCompactSummary lost during encode round-trip")
	}
}
