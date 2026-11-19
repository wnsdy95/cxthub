package sessionrecords

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func claudeContentLine(t *testing.T, kind, uuid string, content any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": kind, "uuid": uuid, "sessionId": "session-1", "timestamp": "2031-01-02T03:00:00.000Z",
		"cwd": "/work/app", "gitBranch": "main", "message": map[string]any{"role": kind, "content": content}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func claudeContentKinds(events []domain.Event) []string {
	out := []string{}
	for _, event := range events {
		out = append(out, string(event.Kind))
	}
	return out
}

func TestClaudeContentReadsTheNamedRecordsRangeByRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-1.jsonl")
	file := claudeContentLine(t, "user", "copied", "this record belongs to an earlier session") +
		claudeContentLine(t, "user", "m1", "commit it") +
		// One record, two events.
		claudeContentLine(t, "assistant", "m2", []any{map[string]any{"type": "text", "text": "running"}, map[string]any{"type": "tool_use", "id": "call-1", "name": "Bash", "input": map[string]any{"command": "git commit"}}}) +
		`{"type":"system","uuid":"s1","subtype":"compact_boundary","timestamp":"2031-01-02T03:00:00.000Z"}` + "\n" +
		claudeContentLine(t, "user", "m3", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "[main abc1234] first"}}) +
		// The same identifier again is the same record; the first line is the record.
		claudeContentLine(t, "user", "m3", "a later line under the same identifier") +
		claudeContentLine(t, "user", "m4", "one more thing") +
		`{"type":"user","uuid":"m5","message":{"content":"still being wr`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path, Ranges: [][]string{{"m1", "m2", "m3"}, {"m4"}}}
	content, err := NewClaudeContent().ReadSessionContent(context.Background(), dir, records)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Missing) != 0 || len(content.Ranges) != 2 {
		t.Fatalf("content = %+v", content)
	}
	if got, want := claudeContentKinds(content.Ranges[0]), []string{"message", "message", "tool_call", "tool_result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first range: %v", got)
	}
	if got := content.Ranges[1]; len(got) != 1 || got[0].Blocks[0].Text != "one more thing" {
		t.Fatalf("second range: %+v", got)
	}
	// Events are numbered across ranges, as one document.
	doc, first, count := content.Document()
	if !reflect.DeepEqual(first, []int{0, 4}) || !reflect.DeepEqual(count, []int{4, 1}) || len(doc.Events) != 5 {
		t.Fatalf("first=%v count=%v events=%d", first, count, len(doc.Events))
	}
	for index, event := range doc.Events {
		if event.Seq != index {
			t.Fatalf("event %d has sequence %d", index, event.Seq)
		}
	}
	if content.Envelope.Cwd != "/work/app" || content.Envelope.GitBranch != "main" {
		t.Fatalf("envelope as read = %+v", content.Envelope)
	}
	// The document names the session, and no folder or branch of this computer.
	if doc.Envelope.SourceProvider != domain.ProviderClaude || doc.Envelope.SessionOriginID != "session-1" || doc.Envelope.Cwd != "" || doc.Envelope.GitBranch != "" {
		t.Fatalf("document envelope = %+v", doc.Envelope)
	}
	if _, err := domain.CanonicalBytes(doc); err != nil {
		t.Fatalf("the document is not canonical CIR: %v", err)
	}
	// Nothing of the copied record, the system record or the unfinished line is in it.
	raw, _ := json.Marshal(doc)
	for _, absent := range []string{"earlier session", "compact_boundary", "still being", "a later line"} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("content holds %q: %s", absent, raw)
		}
	}
}

func TestClaudeContentMasksLikeCapture(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".cxtsecrets"), []byte("synthetic-listed-value-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-1.jsonl")
	file := claudeContentLine(t, "user", "m1", "the value is synthetic-listed-value-123 at https://user:synthetic-password@example.invalid/path") +
		claudeContentLine(t, "user", "m2", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "printed synthetic-listed-value-123"}})
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path, Ranges: [][]string{{"m1"}, {"m2"}}}
	content, err := NewClaudeContent().ReadSessionContent(context.Background(), dir, records)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(content.Ranges)
	if strings.Contains(string(raw), "synthetic-listed-value-123") || strings.Contains(string(raw), "synthetic-password") {
		t.Fatalf("a secret reached the content: %s", raw)
	}
	// Both steps ran: the repository's listed value, and the pattern scrub.
	if !strings.Contains(string(raw), "deleted by security policy") || !strings.Contains(string(raw), "redacted:password") {
		t.Fatalf("masking markers are absent: %s", raw)
	}
}

func TestClaudeContentNamesWhatItCannotReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-1.jsonl")
	if err := os.WriteFile(path, []byte(claudeContentLine(t, "user", "m1", "kept")), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewClaudeContent()
	records := domain.SessionRecordRanges{Provider: domain.ProviderClaude, ID: "session-1", Source: path, Ranges: [][]string{{"m1", "gone"}, {"also-gone"}}}
	content, err := reader.ReadSessionContent(context.Background(), dir, records)
	if err != nil || !reflect.DeepEqual(content.Missing, []string{"gone", "also-gone"}) || len(content.Ranges[0]) != 1 || len(content.Ranges[1]) != 0 {
		t.Fatalf("content = %+v, err = %v", content, err)
	}
	for _, records := range []domain.SessionRecordRanges{
		{Provider: domain.ProviderClaude, ID: "session-1", Ranges: [][]string{{"m1"}}},
		{Provider: domain.ProviderCodex, ID: "thread-1", Source: path, Ranges: [][]string{{"m1"}}},
		{Provider: domain.ProviderClaude, ID: "session-1", Source: filepath.Join(dir, "absent.jsonl"), Ranges: [][]string{{"m1"}}},
	} {
		if _, err := reader.ReadSessionContent(context.Background(), dir, records); err == nil {
			t.Fatalf("%+v was read", records)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.ReadSessionContent(ctx, dir, records); err == nil {
		t.Fatal("a cancelled read must fail")
	}
}
