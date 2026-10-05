//go:build darwin || linux

package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func completedExchangeArchive(t *testing.T, mode string) (*FirstExchange, []map[string]any, string) {
	t.Helper()
	f := newFirstExchangeFixture(t, mode)
	log := filepath.Join(f.opts.Cwd, "resumed.jsonl")
	f.opts.Env = append(f.opts.Env, "CXT_CLAUDE_IDLE_LOG="+log, "CXT_CLAUDE_IDLE_MODE=exit")
	e := f.start(t, true)
	if _, err := e.Run(firstExchangeRunContext(t), "synthetic question", firstExchangeAllow); err != nil {
		t.Fatal(err)
	}
	q := e.s.firstQuestion
	row := func(kind, id, parent string, value any) map[string]any {
		var p any
		if parent != "" {
			p = parent
		}
		return map[string]any{"type": kind, "uuid": id, "parentUuid": p, "sessionId": e.SessionID(), "cwd": e.s.cwd, "isSidechain": false, "message": value}
	}
	ref := row("user", e.s.receipt.MessageID, "", map[string]any{"role": "user", "content": nativeReferencePrefix + "PRIVATE_SYNTHETIC_REFERENCE"})
	ref["isMeta"] = true
	rows := []map[string]any{ref, row("user", q.id, e.s.receipt.MessageID, map[string]any{"role": "user", "content": "synthetic question"})}
	parent := q.id
	// Reuse the independent wire fixture's assistant frames. The archive parser
	// must match all blocks/UUIDs in order, not only the returned visible answer.
	firstExchangeQueryFrames(mode, e.SessionID(), q.id, json.RawMessage(`{"role":"user","content":"synthetic question"}`), func(v any) {
		m := v.(map[string]any)
		if m["type"] != "assistant" {
			return
		}
		id := m["uuid"].(string)
		// Clone now: sibling fixture deliberately reuses its message object.
		raw, _ := json.Marshal(row("assistant", id, parent, m["message"]))
		var archived map[string]any
		_ = json.Unmarshal(raw, &archived)
		rows = append(rows, archived)
		parent = id
	})
	writeExchangeRows(t, e, rows)
	return e, rows, log
}

func writeExchangeRows(t *testing.T, e *FirstExchange, rows []map[string]any) {
	t.Helper()
	path := unitArchive(e.s)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, row := range rows {
		if err := json.NewEncoder(f).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExchangeArchiveExactReadbackAndResume(t *testing.T) {
	for _, mode := range []string{"normal", "two-text-blocks", "sibling-query-omitted"} {
		t.Run(mode, func(t *testing.T) {
			e, _, log := completedExchangeArchive(t, mode)
			path := unitArchive(e.s)
			before, _ := os.ReadFile(path)
			if _, err := e.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("unverified archive prepared", err)
			}
			receipt, err := e.VerifyArchive(context.Background(), path)
			if err != nil || !receipt.Persisted || !receipt.Reference.Persisted || receipt.AnswerHash != hashText(firstExchangeAnswer) || receipt.AssistantRecords != len(e.s.firstQuestion.archiveMessages) {
				t.Fatal("readback", receipt, err)
			}
			if _, err := e.s.VerifyArchive(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("query reused no-query proof", err)
			}
			if _, err := e.s.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("query reused no-query resume", err)
			}
			plan, err := e.PrepareIdleResume(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			in, out, diag := idleFiles(t)
			process, err := plan.Start(context.Background(), in, out, diag)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = process.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := process.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			got := awaitIdleInvocation(t, log)
			want := []string{"--bare", "--model", "sonnet", "--resume", path}
			if !reflect.DeepEqual(got.Args, want) || got.Cwd != e.s.cwd {
				t.Fatal("changed invocation", got.Args)
			}
			assertIdleReaped(t, process)
			if _, err := plan.Start(ctx, in, out, diag); !errors.Is(err, ErrState) {
				t.Fatal("duplicate resume", err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("archive rewritten")
			}
		})
	}
}

func TestExchangeArchiveRejectsChangedConversation(t *testing.T) {
	cases := map[string]func([]map[string]any) []map[string]any{
		"missing-answer":   func(r []map[string]any) []map[string]any { return r[:2] },
		"duplicate-answer": func(r []map[string]any) []map[string]any { return append(r, r[2]) },
		"foreign-session":  func(r []map[string]any) []map[string]any { r[2]["sessionId"] = "foreign"; return r },
		"foreign-cwd":      func(r []map[string]any) []map[string]any { r[2]["cwd"] = "/other"; return r },
		"wrong-parent":     func(r []map[string]any) []map[string]any { r[2]["parentUuid"] = r[0]["uuid"]; return r },
		"sidechain":        func(r []map[string]any) []map[string]any { r[2]["isSidechain"] = true; return r },
		"compact":          func(r []map[string]any) []map[string]any { r[1]["isCompactSummary"] = true; return r },
		"hidden-question":  func(r []map[string]any) []map[string]any { r[1]["isMeta"] = true; return r },
		"changed-reference": func(r []map[string]any) []map[string]any {
			r[0]["message"].(map[string]any)["content"] = "changed"
			return r
		},
		"changed-question": func(r []map[string]any) []map[string]any {
			r[1]["message"].(map[string]any)["content"] = "changed"
			return r
		},
		"changed-response": func(r []map[string]any) []map[string]any {
			r[2]["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "changed"}}
			return r
		},
		"hidden-extra": func(r []map[string]any) []map[string]any {
			m := r[2]["message"].(map[string]any)
			m["content"] = append(m["content"].([]any), map[string]any{"type": "thinking", "thinking": "unrecorded"})
			return r
		},
		"error-response":           func(r []map[string]any) []map[string]any { r[2]["isApiErrorMessage"] = true; return r },
		"hidden-response":          func(r []map[string]any) []map[string]any { r[2]["isMeta"] = true; return r },
		"transcript-only-question": func(r []map[string]any) []map[string]any { r[1]["isVisibleInTranscriptOnly"] = true; return r },
		"null-behavior":            func(r []map[string]any) []map[string]any { r[1]["isVisibleInTranscriptOnly"] = nil; return r },
		"model": func(r []map[string]any) []map[string]any {
			r[2]["message"].(map[string]any)["model"] = "other"
			return r
		},
		"message-id":      func(r []map[string]any) []map[string]any { r[2]["message"].(map[string]any)["id"] = "other"; return r },
		"additional-turn": func(r []map[string]any) []map[string]any { r = append(r, r[1]); return r },
		"unknown-record":  func(r []map[string]any) []map[string]any { return append(r, map[string]any{"type": "future"}) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e, rows, _ := completedExchangeArchive(t, "normal")
			writeExchangeRows(t, e, mutate(rows))
			if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
				t.Fatal("changed conversation accepted")
			}
		})
	}
}

func TestExchangeResumeRechecksArchiveAtLaunch(t *testing.T) {
	e, rows, log := completedExchangeArchive(t, "normal")
	path := unitArchive(e.s)
	if _, err := e.VerifyArchive(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	plan, err := e.PrepareIdleResume(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	rows[2]["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "later corruption"}}
	writeExchangeRows(t, e, rows)
	in, out, diag := idleFiles(t)
	if _, err := plan.Start(context.Background(), in, out, diag); err == nil {
		t.Fatal("changed file launched")
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("process was started", err)
	}
	if _, err := plan.Start(context.Background(), in, out, diag); !errors.Is(err, ErrState) {
		t.Fatal("failed launch retried", err)
	}
}

func TestExchangeArchiveNativeAttachmentChain(t *testing.T) {
	e, rows, _ := completedExchangeArchive(t, "normal")
	attachment := func(id, parent string, value any) map[string]any {
		return map[string]any{"type": "attachment", "uuid": id, "parentUuid": parent, "sessionId": e.SessionID(), "cwd": e.s.cwd, "isSidechain": false, "attachment": value}
	}
	contextRow := attachment("session-context", rows[1]["uuid"].(string), map[string]any{"type": "session_context", "context": map[string]any{"gitStatus": "synthetic native status"}})
	dateRow := attachment("date", "session-context", map[string]any{"type": "date", "date": "2026-10-05"})
	rows[2]["parentUuid"] = "date"
	rows = append(rows[:2], contextRow, dateRow, rows[2])
	writeExchangeRows(t, e, rows)
	receipt, err := e.VerifyArchive(context.Background(), unitArchive(e.s))
	if err != nil || len(receipt.NativeAttachments) != 2 || receipt.NativeAttachments[0].Type != "session_context" || receipt.NativeAttachments[0].ContentHash == "" {
		t.Fatal("attachment chain not classified", err)
	}
	// Native additions are preserved, not stripped to manufacture a direct edge.
	dateRow["parentUuid"] = rows[1]["uuid"]
	writeExchangeRows(t, e, rows)
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
		t.Fatal("disconnected native context accepted")
	}
}

func TestExchangeArchiveRejectsInvalidLifecycle(t *testing.T) {
	e := newFirstExchangeFixture(t, "normal").start(t, true)
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); !errors.Is(err, ErrState) {
		t.Fatal("live session proof", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); !errors.Is(err, ErrState) {
		t.Fatal("no response proof", err)
	}
	good, _, _ := completedExchangeArchive(t, "normal")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := good.VerifyArchive(ctx, unitArchive(good.s)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
	path := unitArchive(good.s)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":`)
	_ = f.Close()
	if _, err := good.VerifyArchive(context.Background(), path); err == nil {
		t.Fatal("partial final record accepted")
	}
}
