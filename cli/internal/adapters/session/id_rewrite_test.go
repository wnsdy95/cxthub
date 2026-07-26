package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRewriteCodexSessionID — session_meta.payload.id only changes to new id, other rows preserved as bytes.
func TestRewriteCodexSessionID(t *testing.T) {
	raw := `{"type":"session_meta","payload":{"id":"old-1234","cwd":"/x"}}
{"type":"response_item","payload":{"type":"message","content":[{"type":"text","text":"old-1234 mentioned"}]}}`
	got := string(rewriteCodexSessionID([]byte(raw), "new-5678"))
	if !strings.Contains(got, `"id":"new-5678"`) {
		t.Fatalf("session_meta.id rewrite failed: %s", got)
	}
	if !strings.Contains(got, "old-1234 mentioned") {
		t.Fatalf("body (conversation content) corrupted: %s", got)
	}
	if strings.Count(got, "new-5678") != 1 {
		t.Fatalf("new id injected outside session_meta: %s", got)
	}
}

// TestNativizeClaudeSession — sessionId rewrite + message record with uuid/parentUuid linear chain.
// If no chain, assumes claude resume has empty conversation (context window scan failure).
func TestNativizeClaudeSession(t *testing.T) {
	raw := `{"type":"user","sessionId":"old-1234","message":{"role":"user","content":"first question"}}
{"type":"assistant","sessionId":"old-1234","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}
{"type":"user","sessionId":"old-1234","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	got := nativizeClaudeSession([]byte(raw), "new-5678")
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 {
		t.Fatalf("Row count changed: %d", len(lines))
	}
	var prev string
	for i, line := range lines {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("Failed to parse row %d: %v", i, err)
		}
		if rec["sessionId"] != "new-5678" {
			t.Fatalf("Failed to rewrite sessionId in row %d: %v", i, rec["sessionId"])
		}
		u, _ := rec["uuid"].(string)
		if u == "" {
			t.Fatalf("Row %d has no uuid", i)
		}
		if i == 0 {
			if v, ok := rec["parentUuid"]; !ok || v != nil {
				t.Fatalf("First record parentUuid must be null: %v", v)
			}
		} else if rec["parentUuid"] != prev {
			t.Fatalf("Row %d parentUuid chain broken: got %v want %s", i, rec["parentUuid"], prev)
		}
		prev = u
	}
	if !strings.Contains(string(got), "first question") || !strings.Contains(string(got), "response") {
		t.Fatalf("Body corruption: %s", got)
	}
}

// TestRewriteClaudeSessionID — Replace only the sessionId field, preserving the body uuid.
func TestRewriteClaudeSessionID(t *testing.T) {
	raw := `{"type":"user","sessionId":"old-1234","message":{"role":"user","content":"discussion of session old-1234"}}
{"type":"assistant","sessionId":"old-1234","message":{"role":"assistant","content":[]}}`
	got := string(rewriteClaudeSessionID([]byte(raw), "new-5678"))
	if strings.Count(got, `"sessionId":"new-5678"`) != 2 {
		t.Fatalf("Failed to rewrite sessionId: %s", got)
	}
	if !strings.Contains(got, "discussion of session old-1234") {
		t.Fatalf("Body (conversation content) is corrupted: %s", got)
	}
}
