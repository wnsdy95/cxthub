package capture

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func msgEvent(text string) domain.Event {
	return domain.Event{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}

// TestScrubStandardPatterns ensures known credential formats are replaced with «redacted:*» markers.
func TestScrubStandardPatterns(t *testing.T) {
	cases := []struct{ in, wantGone, wantKind string }{
		{"aws key is AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE", "aws-key"},
		{"token: ghp_" + strings.Repeat("a1B2", 9), "ghp_", "github-token"},
		{"pat github_pat_11ABCDEFG0123456789012_tail", "github_pat_11ABCDEFG", "github-token"},
		{"openai sk-proj-abcdefghijklmnopqrstuvwx", "sk-proj-abcdefghijklmnop", "api-key"},
		{"slack xoxb-1234567890-abcdefghijk", "xoxb-1234567890", "slack-token"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.SflKxwRJSMeKKF2QT4fwpM", "SflKxwRJSMeKKF2QT4fwpM", "jwt"}, // gitleaks:allow -- synthetic scrubber fixture
		{"Authorization: Bearer abcdef0123456789TOKENVALUE", "abcdef0123456789TOKENVALUE", "bearer"},
		{"clone https://user:hunter22pass@github.com/x/y.git", "hunter22pass", "password"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", "MIIabc", "private-key"},
	}
	for _, c := range cases {
		out := Scrub(domain.CIRDocument{Events: []domain.Event{msgEvent(c.in)}}, ScrubStandard)
		got := out.Events[0].Blocks[0].Text
		if strings.Contains(got, c.wantGone) {
			t.Errorf("[%s] secret unmasked: %q", c.wantKind, got)
		}
		if !strings.Contains(got, "«redacted:"+c.wantKind+"»") {
			t.Errorf("[%s] missing redacted marker: %q", c.wantKind, got)
		}
	}
}

// TestScrubPreservesLockedAndHashes ensures that (1) locked original (signature/ciphertext) is never modified, and (2) hex hashes (git sha/sha256) in development conversations are preserved in strict mode.
func TestScrubPreservesLockedAndHashes(t *testing.T) {
	blob := "CAISkwUKYggPGAIqQO5dgL5KFfwin45o04XskuLmQw3awagKAEgEo7Kw"
	hash := "sha256:9b23182f6824fa499eb8de987f3df954795faf55304c7dfd864881ad49d9abba"
	doc := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventReasoning, Locked: &domain.LockedBlob{Provider: "claude", Scheme: "signature", Blob: blob}, RedactedSummary: "summary with " + hash},
		msgEvent("refer to commit " + hash),
	}}
	out := Scrub(doc, ScrubStrict)
	if out.Events[0].Locked.Blob != blob {
		t.Fatal("locked.blob modified — full-fidelity rehydration breaks")
	}
	if !strings.Contains(out.Events[0].RedactedSummary, hash) || !strings.Contains(out.Events[1].Blocks[0].Text, hash) {
		t.Fatal("hex hash(git/sha256) masked — dev conversation body destroyed")
	}
}

// TestScrubStrictEnvAndEntropy — strict only: secret env injection + high entropy base64.
func TestScrubStrictEnvAndEntropy(t *testing.T) {
	in := "export DB_PASSWORD=supersecret99 and PATH=/usr/bin should persist, token qQxZ7mK2pL9vB4nR8sT1wY5uH0jD6fG3aC+eIoP/kMlN="
	out := Scrub(domain.CIRDocument{Events: []domain.Event{msgEvent(in)}}, ScrubStrict)
	got := out.Events[0].Blocks[0].Text
	if strings.Contains(got, "supersecret99") {
		t.Errorf("env secret not masked: %q", got)
	}
	if !strings.Contains(got, "PATH=/usr/bin") {
		t.Errorf("excessive masking of general env: %q", got)
	}
	if strings.Contains(got, "qQxZ7mK2pL9vB4nR8sT1wY5uH0jD6fG3aC+eIoP/kMlN=") {
		t.Errorf("high entropy token not masked: %q", got)
	}
	// standard does not touch high entropy (conservative default).
	out2 := Scrub(domain.CIRDocument{Events: []domain.Event{msgEvent(in)}}, ScrubStandard)
	if !strings.Contains(out2.Events[0].Blocks[0].Text, "qQxZ7mK2pL9vB4nR8sT1wY5uH0jD6fG3aC+eIoP/kMlN=") {
		t.Error("standard masks high entropy (must be strict only)")
	}
}

// TestScrubToolIO verifies that nested values in tool_call input and tool_result output are scrubbed too.
func TestScrubToolIO(t *testing.T) {
	doc := domain.CIRDocument{Events: []domain.Event{
		{Kind: domain.EventToolCall, Input: map[string]interface{}{"command": "curl -H 'Authorization: Bearer abcdef0123456789TOKENVALUE'"}}, // gitleaks:allow -- synthetic scrubber fixture
		{Kind: domain.EventToolResult, Output: map[string]interface{}{"stdout": []interface{}{"AKIAIOSFODNN7EXAMPLE"}}},
	}}
	out := Scrub(doc, ScrubStandard)
	if s := out.Events[0].Input["command"].(string); strings.Contains(s, "TOKENVALUE") {
		t.Errorf("tool input was not scrubbed: %q", s)
	}
	if s := out.Events[1].Output.(map[string]interface{})["stdout"].([]interface{})[0].(string); strings.Contains(s, "AKIA") {
		t.Errorf("tool output was not scrubbed: %q", s)
	}
}

// TestScrubOff verifies that off mode changes nothing.
func TestScrubOff(t *testing.T) {
	doc := domain.CIRDocument{Events: []domain.Event{msgEvent("AKIAIOSFODNN7EXAMPLE")}}
	out := Scrub(doc, ScrubOff)
	if out.Events[0].Blocks[0].Text != "AKIAIOSFODNN7EXAMPLE" {
		t.Fatal("off tier performs masking")
	}
}
