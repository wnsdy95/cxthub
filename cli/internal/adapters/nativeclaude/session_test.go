//go:build darwin || linux

package nativeclaude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func TestMain(m *testing.M) {
	if os.Getenv("CXT_CLAUDE_UNIT_HELPER") == "1" {
		claudeUnitHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func claudeUnitHelper() {
	mode := os.Getenv("CXT_CLAUDE_UNIT_MODE")
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			if link := os.Getenv("CXT_CLAUDE_UNIT_LINK"); link != "" {
				_ = os.Remove(link)
				_ = os.Symlink(os.Getenv("CXT_CLAUDE_UNIT_REPLACEMENT"), link)
			}
			if mode == "wrong-version" {
				fmt.Println("2.1.287 (Claude Code)")
			} else {
				fmt.Println("2.1.285 (Claude Code)")
			}
			return
		}
	}
	var sid string
	for i, arg := range os.Args {
		if arg == "--session-id" && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
	}
	cwd, _ := os.Getwd()
	total := int64(40)
	write := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	control := func(id string, value any) {
		write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": value}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for scanner.Scan() {
		m, _ := object(scanner.Bytes())
		var kind string
		_ = json.Unmarshal(m["type"], &kind)
		if kind == "control_request" {
			r, _ := object(m["request"])
			var method, id string
			_ = json.Unmarshal(r["subtype"], &method)
			_ = json.Unmarshal(m["request_id"], &id)
			if method == "initialize" {
				control(id, map[string]any{"models": []any{map[string]any{"value": "sonnet", "resolvedModel": "fixture-model"}}, "account": map[string]any{"private": "PRIVATE_ACCOUNT"}})
				write(map[string]any{"type": "system", "subtype": "commands_changed", "session_id": sid, "uuid": "metadata", "commands": []any{map[string]any{"name": "fixture", "description": "PRIVATE_DESCRIPTION"}}})
				write(map[string]any{"type": "system", "subtype": "session_title_changed", "session_id": sid, "title": "PRIVATE_TITLE"})
				if mode == "blocked-write" {
					time.Sleep(time.Minute)
					return
				}
			} else if method == "get_context_usage" {
				var detail string
				_ = json.Unmarshal(r["detail"], &detail)
				if detail != "summary" {
					os.Exit(9)
				}
				control(id, map[string]any{"model": "fixture-model", "totalTokens": total, "maxTokens": 1000000, "rawMaxTokens": 1000000, "isAutoCompactEnabled": true, "autoCompactThreshold": 967000, "apiUsage": nil})
			} else {
				os.Exit(10)
			}
			continue
		}
		if kind != "user" {
			os.Exit(11)
		}
		var query, synthetic, composed bool
		_ = json.Unmarshal(m["shouldQuery"], &query)
		_ = json.Unmarshal(m["isSynthetic"], &synthetic)
		_ = json.Unmarshal(m["client_composed"], &composed)
		if query || !synthetic || !composed {
			os.Exit(12)
		}
		id, _ := stringField(m, "uuid")
		got, _ := stringField(m, "session_id")
		if got != sid {
			os.Exit(13)
		}
		text, err := referenceText(m["message"])
		if err != nil {
			os.Exit(14)
		}
		if mode == "no-ack" {
			time.Sleep(time.Minute)
			return
		}
		lifecycle := func(state string) {
			write(map[string]any{"type": "command_lifecycle", "session_id": sid, "command_uuid": id, "state": state})
		}
		lifecycle("queued")
		lifecycle("started")
		if mode == "replay" {
			write(map[string]any{"type": "user", "uuid": id, "session_id": sid, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": text}})
		}
		path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", providerfs.EncodeCwd(cwd), sid+".jsonl")
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		archive, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "sessionId": sid, "cwd": cwd, "message": map[string]any{"role": "user", "content": "[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]\n" + text}})
		_ = os.WriteFile(path, append(archive, '\n'), 0600)
		result := zeroResultFixture(sid, id)
		if mode == "model-turn" {
			result["num_turns"] = 1
		}
		if mode == "wrong-message" {
			result["user_message_uuid"] = "other"
		}
		write(result)
		lifecycle("completed")
		total += int64(len(text))
	}
	if mode == "late-assistant" {
		write(map[string]any{"type": "assistant", "session_id": sid, "message": map[string]any{"content": "PRIVATE_MODEL_BODY"}})
	}
	if mode == "late-compaction" {
		write(map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": sid})
	}
	if mode == "truncated-eof" {
		fmt.Fprint(os.Stdout, `{"type":"system"}`)
	}
}

func zeroResultFixture(sid, id string) map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": sid, "user_message_uuid": id, "user_message_uuids": []string{id}, "num_turns": 0, "duration_api_ms": 0, "total_cost_usd": 0, "queued_turn_count": 0, "permission_denials": []any{}, "modelUsage": map[string]any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}
}

func unitOptions(t *testing.T, mode string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{Executable: exe, Cwd: root, Model: "sonnet", Env: []string{"HOME=" + root, "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "CXT_CLAUDE_UNIT_HELPER=1", "CXT_CLAUDE_UNIT_MODE=" + mode, "GORACE=atexit_sleep_ms=0"}}
}
func unitSession(t *testing.T, mode string) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	s, err := Start(ctx, unitOptions(t, mode))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func unitArchive(s *Session) string {
	return filepath.Join(s.archiveRoot, providerfs.EncodeCwd(s.cwd), s.id+".jsonl")
}

func TestSessionNoTurnAcknowledgmentAndExactArchive(t *testing.T) {
	for _, mode := range []string{"normal", "replay"} {
		t.Run(mode, func(t *testing.T) {
			s := unitSession(t, mode)
			ctx := context.Background()
			before, err := s.ContextSummary(ctx)
			if err != nil || before.Measurement != "local_estimate" || before.TotalTokens != 40 {
				t.Fatal(before, err)
			}
			models := s.Models()
			models[0].Value = "changed"
			if s.Models()[0].Value != "sonnet" {
				t.Fatal("catalog aliases mutable caller slice")
			}
			text := "PRIVATE_REFERENCE @path /command \u97d3\u6587"
			r, err := s.AppendReference(ctx, text)
			if err != nil {
				t.Fatal(err)
			}
			if !r.NoTurnAcknowledged || r.ReplayAcknowledged != (mode == "replay") || r.Persisted || r.PayloadHash != hashText(text) || r.UTF8Bytes != len(text) || r.NativeContentHash != hashText("[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]\n"+text) || r.NativeUTF8Bytes != len(text)+48 || r.ProviderAcceptance != "unverified" {
				t.Fatalf("false receipt: %+v", r)
			}
			if _, err := s.VerifyArchive(ctx, unitArchive(s)); !errors.Is(err, ErrState) {
				t.Fatal("archive checked before EOF")
			}
			if _, err := s.AppendReference(ctx, text); !errors.Is(err, ErrState) {
				t.Fatal("second append allowed")
			}
			after, err := s.ContextSummary(ctx)
			if err != nil || after.TotalTokens <= before.TotalTokens {
				t.Fatal(after, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ContextSummary(ctx); !errors.Is(err, ErrClosed) {
				t.Fatal("closed session reused", err)
			}
			r, err = s.VerifyArchive(ctx, unitArchive(s))
			if err != nil || !r.Persisted {
				t.Fatal("exact archive not confirmed", err)
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("receipt leaked input")
			}
			if _, err := os.Stat(unitArchive(s)); err != nil {
				t.Fatal("archive removed on close")
			}
		})
	}
}

func TestSessionRejectsActivityAndLateShutdownFrames(t *testing.T) {
	for _, mode := range []string{"model-turn", "wrong-message", "late-assistant", "late-compaction", "truncated-eof"} {
		t.Run(mode, func(t *testing.T) {
			s := unitSession(t, mode)
			r, err := s.AppendReference(context.Background(), "PRIVATE_HISTORY")
			if strings.HasPrefix(mode, "late-") || mode == "truncated-eof" {
				if err != nil || !r.NoTurnAcknowledged {
					t.Fatal(err)
				}
				err = s.Close()
			}
			if err == nil || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
				t.Fatal("activity ignored or private error leaked", err)
			}
			if _, err := s.VerifyArchive(context.Background(), unitArchive(s)); err == nil {
				t.Fatal("failed EOF audit attested persistence")
			}
		})
	}
}

func TestSessionCancellationAbandonsUncertainAppend(t *testing.T) {
	for _, mode := range []string{"no-ack", "blocked-write"} {
		t.Run(mode, func(t *testing.T) {
			s := unitSession(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := s.AppendReference(ctx, strings.Repeat("x", MaxReferenceBytes))
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 4*time.Second {
				t.Fatal("cancel failed to bound append", err)
			}
			if _, err := s.AppendReference(context.Background(), "retry"); err == nil {
				t.Fatal("uncertain append retried")
			}
			if _, err := s.VerifyArchive(context.Background(), unitArchive(s)); err == nil {
				t.Fatal("uncertain append claimed persistence")
			}
		})
	}
}

func TestSessionPinsExecutableSymlinkBeforeVersion(t *testing.T) {
	opts := unitOptions(t, "normal")
	link := filepath.Join(opts.Cwd, "launch")
	replacement := filepath.Join(opts.Cwd, "replacement")
	marker := filepath.Join(opts.Cwd, "wrong-executable")
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(opts.Executable, link); err != nil {
		t.Fatal(err)
	}
	opts.Executable = link
	opts.Env = append(opts.Env, "CXT_CLAUDE_UNIT_LINK="+link, "CXT_CLAUDE_UNIT_REPLACEMENT="+replacement)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ContextSummary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("launch followed replaced version-check symlink")
	}
}

func TestSessionRejectsUnsupportedVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if s, err := Start(ctx, unitOptions(t, "wrong-version")); err == nil {
		_ = s.Close()
		t.Fatal("unsupported version accepted")
	}
}
