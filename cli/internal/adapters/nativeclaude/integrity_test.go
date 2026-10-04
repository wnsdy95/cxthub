//go:build darwin || linux

package nativeclaude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestArchiveRequiresExactNativeProjection(t *testing.T) {
	s := unitSession(t, "normal")
	const text = "synthetic archived context"
	if _, err := s.AppendReference(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := unitArchive(s)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, transform := range map[string]func(map[string]any){
		"missing provenance": func(row map[string]any) { row["message"].(map[string]any)["content"] = text },
		"additional text": func(row map[string]any) {
			row["message"].(map[string]any)["content"] = nativeReferencePrefix + text + " extra"
		},
		"wrong role":    func(row map[string]any) { row["message"].(map[string]any)["role"] = "assistant" },
		"wrong session": func(row map[string]any) { row["sessionId"] = "foreign-session" },
		"compacted":     func(row map[string]any) { row["isCompactSummary"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			var row map[string]any
			if err := json.Unmarshal(valid, &row); err != nil {
				t.Fatal(err)
			}
			transform(row)
			raw, _ := json.Marshal(row)
			if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.VerifyArchive(context.Background(), path); err == nil {
				t.Fatal("modified archive attested as exact")
			}
		})
	}
	if err := os.WriteFile(path, append(append([]byte{}, valid...), valid...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("duplicate persisted message accepted")
	}
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(context.Background(), outside); err == nil {
		t.Fatal("foreign archive accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("escaping symlink accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := s.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("FIFO accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("archive FIFO blocked")
	}
}

func TestProtocolRejectsAmbiguousOrActiveInput(t *testing.T) {
	for _, raw := range []string{
		`{"type":"system","type":"user"}`,
		`{"type":"system","subtype":"status","status":"requesting"}`,
		`{"type":"system","subtype":"compact_boundary"}`,
		`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool"}}`,
		`{"type":"assistant","message":{"content":"synthetic model output"}}`,
		`{"type":"system","subtype":"commands_changed","session_id":"foreign","commands":[]}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"wrong","response":{}}}`,
		`{"type":"command_lifecycle","session_id":"owned","command_uuid":"message","state":"completed"}`,
	} {
		s := &Session{id: "owned", appended: true, receipt: ReferenceReceipt{MessageID: "message"}, pending: &pendingCall{id: "request", kind: "initialize", result: make(chan json.RawMessage, 1)}}
		if err := s.frame([]byte(raw)); err == nil {
			t.Fatalf("ambiguous/activity frame accepted: %s", raw)
		}
	}
	for _, change := range []func(map[string]any){
		func(row map[string]any) { row["num_turns"] = 1 },
		func(row map[string]any) { row["duration_api_ms"] = 1 },
		func(row map[string]any) { row["is_error"] = true },
		func(row map[string]any) { row["usage"].(map[string]any)["input_tokens"] = 1 },
		func(row map[string]any) { row["usage"].(map[string]any)["output_tokens"] = nil },
		func(row map[string]any) { row["modelUsage"] = map[string]any{"model": map[string]any{}} },
		func(row map[string]any) { row["queued_turn_count"] = 1 },
	} {
		row := zeroResultFixture("owned", "message")
		change(row)
		raw, _ := json.Marshal(row)
		obj, _ := object(raw)
		if err := zeroTurnResult(obj); err == nil {
			t.Fatal("nonzero/ambiguous model result accepted")
		}
	}
	if err := readFrames(strings.NewReader(`{"type":"system"}`), func([]byte) error { return nil }); err == nil {
		t.Fatal("truncated frame accepted at EOF")
	}
}
