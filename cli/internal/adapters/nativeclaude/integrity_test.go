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
	e, rows, _ := completedExchangeArchive(t, "normal")
	const text = "PRIVATE_SYNTHETIC_REFERENCE"
	path := unitArchive(e.s)
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
			raw, _ := json.Marshal(rows)
			var changed []map[string]any
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			transform(changed[0])
			writeExchangeRows(t, e, changed)
			if _, err := e.VerifyArchive(context.Background(), path); err == nil {
				t.Fatal("modified archive attested as exact")
			}
		})
	}
	if err := os.WriteFile(path, append(append([]byte{}, valid...), valid...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("duplicate persisted message accepted")
	}
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), outside); err == nil {
		t.Fatal("foreign archive accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("escaping symlink accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := e.VerifyArchive(context.Background(), path); err == nil {
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

func TestReferenceAcknowledgmentRejectsNestedUsage(t *testing.T) {
	for _, field := range []string{"cache_creation", "output_tokens_details", "server_tool_use"} {
		t.Run(field, func(t *testing.T) {
			for _, raw := range []string{`{"count":0}`, `{"count":1}`, `{"count":null}`, `{"count":"0"}`, `null`, `[]`} {
				row := zeroResultFixture("owned", "reference")
				row["usage"].(map[string]any)[field] = json.RawMessage(raw)
				encoded, _ := json.Marshal(row)
				m, err := object(encoded)
				if err != nil {
					t.Fatal(err)
				}
				err = zeroTurnResult(m)
				if (err == nil) != (raw == `{"count":0}`) {
					t.Fatalf("nested usage was not a typed zero: %s: %v", raw, err)
				}
			}
		})
	}
}
