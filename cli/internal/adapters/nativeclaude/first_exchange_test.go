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
	"sync/atomic"
	"testing"
	"time"
)

// This dispatch precedes TestMain and never launches Claude. Each helper has a
// fresh environment, an owned recorder, no descendants, and a finite lifetime.
func init() {
	if os.Getenv("CXT_FIRST_EXCHANGE_UNIT_HELPER") == "1" {
		time.AfterFunc(8*time.Second, func() { os.Exit(98) })
		os.Exit(firstExchangeUnitHelper())
	}
}

const firstExchangeAnswer = "Synthetic text response."

func firstExchangeUnitHelper() int {
	for _, arg := range os.Args[1:] {
		if arg == "--resume" {
			idleUnitHelper()
			return 0
		}
	}
	mode := os.Getenv("CXT_FIRST_EXCHANGE_UNIT_MODE")
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			version := "2.1.287"
			if mode == "old-version" {
				version = "2.1.285"
			}
			fmt.Println(version + " (Claude Code)")
			return 0
		}
	}
	var sid, permissionPrompts string
	for i, arg := range os.Args {
		if arg == "--session-id" && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
		if arg == "--permission-prompts" && i+1 < len(os.Args) {
			permissionPrompts = os.Args[i+1]
		}
	}
	wantPrompts := "host"
	if mode == "old-version" {
		wantPrompts = "none"
	}
	if sid == "" || permissionPrompts != wantPrompts {
		return 90
	}
	recorder, err := os.OpenFile(os.Getenv("CXT_FIRST_EXCHANGE_UNIT_RECORD"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 91
	}
	defer recorder.Close()
	write := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	control := func(id string, value any) {
		write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": value}})
	}
	lifecycle := func(id, state string) {
		write(map[string]any{"type": "command_lifecycle", "session_id": sid, "command_uuid": id, "state": state})
	}
	referenced, summaries := false, 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		raw := append(append([]byte{}, scanner.Bytes()...), '\n')
		if _, err := recorder.Write(raw); err != nil {
			return 92
		}
		m, err := object(scanner.Bytes())
		if err != nil {
			return 93
		}
		kind, _ := stringField(m, "type")
		if kind == "control_request" {
			r, _ := object(m["request"])
			method, _ := stringField(r, "subtype")
			id, _ := stringField(m, "request_id")
			switch method {
			case "initialize":
				if mode == "host-notifications" {
					m := hostNotification("ui_invalidate")
					m["session_id"] = sid
					write(m)
				}
				control(id, map[string]any{"models": []any{map[string]any{"value": "sonnet", "resolvedModel": "fixture-model"}}})
			case "get_context_usage":
				if string(r["detail"]) != `"summary"` {
					return 94
				}
				total := 40
				if referenced {
					total = 57
					summaries++
				}
				value := map[string]any{"model": "fixture-model", "totalTokens": total, "maxTokens": 1000000, "rawMaxTokens": 1000000, "isAutoCompactEnabled": true, "autoCompactThreshold": 967000, "apiUsage": nil}
				if referenced && summaries >= 2 {
					switch mode {
					case "summary-total-drift":
						value["totalTokens"] = 58
					case "summary-model-drift":
						value["model"] = "other-model"
					case "summary-window-drift":
						value["maxTokens"] = 200000
					case "summary-threshold-drift":
						value["autoCompactThreshold"] = 900000
					}
				}
				if mode == "summary-used" && referenced {
					value["apiUsage"] = map[string]any{"input_tokens": 1}
				}
				control(id, value)
			default:
				return 95
			}
			continue
		}
		if kind != "user" {
			return 96 // In particular, never accept an automatic permission reply.
		}
		id, _ := stringField(m, "uuid")
		query, err := boolean(m, "shouldQuery")
		if err != nil {
			return 97
		}
		if !query {
			referenced = true
			lifecycle(id, "queued")
			lifecycle(id, "started")
			result := zeroResultFixture(sid, id)
			result["result_index"] = 0
			if mode == "bad-reference" {
				result["num_turns"] = 1
			}
			switch mode {
			case "reference-index-missing":
				delete(result, "result_index")
			case "reference-index-wrong":
				result["result_index"] = 1
			case "reference-index-null":
				result["result_index"] = nil
			case "reference-index-string":
				result["result_index"] = "0"
			}
			write(result)
			lifecycle(id, "completed")
			continue
		}
		if mode == "silent-query" {
			continue // EOF still terminates promptly when the caller cancels.
		}
		if mode == "host-notifications" {
			m := hostNotification("informational")
			m["session_id"] = sid
			write(m)
		}
		firstExchangeQueryFrames(mode, sid, id, m["message"], write)
	}
	if scanner.Err() != nil {
		return 99
	}
	switch mode {
	case "host-notifications":
		m := hostNotification("ui_invalidate")
		m["session_id"] = sid
		write(m)
	case "late-assistant":
		write(map[string]any{"type": "assistant", "session_id": sid, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "PRIVATE_LATE_RESPONSE"}}}})
	case "late-compaction":
		write(map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": sid})
	case "late-truncated":
		fmt.Fprint(os.Stdout, `{"type":"result"`)
	}
	return 0
}

func firstExchangeQueryFrames(mode, sid, id string, message json.RawMessage, write func(any)) {
	lifecycle := func(state string) {
		row := map[string]any{"type": "command_lifecycle", "session_id": sid, "command_uuid": id, "state": state}
		if mode == "foreign-lifecycle" {
			row["command_uuid"] = "foreign-command"
		}
		write(row)
	}
	if mode == "started-before-queued" {
		lifecycle("started")
		return
	}
	lifecycle("queued")
	if mode == "duplicate-queued" {
		lifecycle("queued")
		return
	}
	if mode != "missing-started" {
		lifecycle("started")
	}
	if mode == "completed-before-result" {
		lifecycle("completed")
		return
	}
	echo := map[string]any{"type": "user", "session_id": sid, "uuid": id, "parent_tool_use_id": nil, "message": message}
	switch mode {
	case "foreign-echo":
		echo["uuid"] = "foreign-message"
	case "changed-echo":
		echo["message"] = map[string]any{"role": "user", "content": "PRIVATE_CHANGED_QUESTION"}
	case "echo-parent":
		echo["parent_tool_use_id"] = "foreign-tool"
	}
	if mode != "missing-echo" {
		write(echo)
	}
	if mode == "duplicate-echo" {
		write(echo)
		return
	}
	if mode == "permission" || mode == "unknown-control" {
		subtype := "can_use_tool"
		if mode == "unknown-control" {
			subtype = "future_permission"
		}
		write(map[string]any{"type": "control_request", "request_id": "synthetic-permission", "request": map[string]any{"subtype": subtype, "tool_name": "PRIVATE_TOOL", "input": map[string]any{"private": "PRIVATE_ARGUMENT"}}})
		return
	}
	if mode == "unknown-event" {
		write(map[string]any{"type": "future_event", "session_id": sid, "private": "PRIVATE_EVENT"})
		return
	}
	if mode == "malformed-json" {
		fmt.Fprintln(os.Stdout, `{"type":"result","type":"result"}`)
		return
	}
	if mode == "status-requesting-and-null" {
		write(map[string]any{"type": "system", "subtype": "status", "session_id": sid, "uuid": "synthetic-requesting", "status": "requesting"})
		write(map[string]any{"type": "system", "subtype": "status", "session_id": sid, "uuid": "synthetic-status-clear", "status": nil})
	}
	if mode == "status-compacting" {
		write(map[string]any{"type": "system", "subtype": "status", "session_id": sid, "uuid": "synthetic-compacting", "status": "compacting"})
		return
	}
	content := []any{map[string]any{"type": "text", "text": firstExchangeAnswer}}
	if mode == "two-text-blocks" {
		content = []any{map[string]any{"type": "text", "text": "Synthetic "}, map[string]any{"type": "text", "text": "text response."}}
	}
	if mode == "tool-use" {
		content = []any{map[string]any{"type": "tool_use", "id": "synthetic-tool", "name": "PRIVATE_TOOL", "input": map[string]any{}}}
	}
	assistant := map[string]any{"type": "assistant", "session_id": sid, "uuid": "synthetic-assistant", "user_message_uuid": id, "parent_tool_use_id": nil, "message": map[string]any{"id": "synthetic-response", "type": "message", "role": "assistant", "model": "fixture-model", "content": content, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 4, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}}
	switch mode {
	case "assistant-query-missing":
		delete(assistant, "user_message_uuid")
	case "assistant-query-foreign":
		assistant["user_message_uuid"] = "foreign-question"
	case "assistant-query-null":
		assistant["user_message_uuid"] = nil
	case "assistant-message-id-missing":
		delete(assistant["message"].(map[string]any), "id")
	case "assistant-message-id-empty":
		assistant["message"].(map[string]any)["id"] = ""
	}
	if strings.HasPrefix(mode, "sibling-") {
		assistant["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "Synthetic "}}
		write(assistant)
		assistant["uuid"] = "synthetic-assistant-sibling"
		assistant["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "text response."}}
		switch mode {
		case "sibling-query-omitted":
			delete(assistant, "user_message_uuid")
		case "sibling-query-foreign":
			assistant["user_message_uuid"] = "foreign-question"
		case "sibling-query-null":
			assistant["user_message_uuid"] = nil
		case "sibling-message-id-foreign":
			assistant["message"].(map[string]any)["id"] = "foreign-response"
		case "sibling-message-id-missing":
			delete(assistant["message"].(map[string]any), "id")
		}
	}
	write(assistant)
	result := zeroResultFixture(sid, id)
	result["result_index"] = 1
	result["num_turns"] = 1
	result["duration_api_ms"] = 1
	result["result"] = firstExchangeAnswer
	// Native projects only the last text block of the latest assistant record
	// into result.result; the adapter's Answer retains all visible text.
	if strings.HasPrefix(mode, "sibling-") || mode == "two-text-blocks" {
		result["result"] = "text response."
	}
	result["terminal_reason"] = "completed"
	result["stop_reason"] = "end_turn"
	result["usage"] = map[string]any{"input_tokens": 12, "output_tokens": 4, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	result["modelUsage"] = map[string]any{"fixture-model": map[string]any{"inputTokens": 12, "outputTokens": 4, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0, "webSearchRequests": 0, "costUSD": 0, "contextWindow": 1000000, "maxOutputTokens": 64000}}
	switch mode {
	case "foreign-session":
		result["session_id"] = "foreign-session"
	case "foreign-result":
		result["user_message_uuid"] = "foreign-message"
	case "foreign-result-list":
		result["user_message_uuids"] = []string{id, "foreign-message"}
	case "result-index-missing":
		delete(result, "result_index")
	case "result-index-reused":
		result["result_index"] = 0
	case "result-index-skipped":
		result["result_index"] = 2
	case "result-index-null":
		result["result_index"] = nil
	case "result-index-string":
		result["result_index"] = "1"
	case "stop-reason-missing":
		delete(result, "stop_reason")
	case "stop-reason-wrong":
		result["stop_reason"] = "max_tokens"
	case "terminal-reason-missing":
		delete(result, "terminal_reason")
	case "terminal-reason-null":
		result["terminal_reason"] = nil
	case "terminal-hook-stopped":
		result["terminal_reason"] = "hook_stopped"
	case "terminal-tool-deferred":
		result["terminal_reason"] = "tool_deferred"
	case "terminal-max-turns":
		result["terminal_reason"] = "max_turns"
	case "terminal-background-requested":
		result["terminal_reason"] = "background_requested"
	case "error-result":
		result["is_error"] = true
		result["errors"] = []string{"PRIVATE_PROVIDER_ERROR"}
	case "zero-turns":
		result["num_turns"] = 0
	case "multiple-turns":
		result["num_turns"] = 2
	case "empty-answer":
		result["result"] = ""
	case "missing-result-usage":
		delete(result, "usage")
	case "negative-input":
		result["usage"].(map[string]any)["input_tokens"] = -1
	case "null-input":
		result["usage"].(map[string]any)["input_tokens"] = nil
	case "string-output":
		result["usage"].(map[string]any)["output_tokens"] = "4"
	case "zero-output":
		result["usage"].(map[string]any)["output_tokens"] = 0
	case "foreign-model":
		result["modelUsage"] = map[string]any{"other-model": map[string]any{"outputTokens": 4}}
	case "queued-turn":
		result["queued_turn_count"] = 1
	case "denied-permission":
		result["permission_denials"] = []any{map[string]any{"tool_name": "PRIVATE_TOOL"}}
	}
	write(result)
	if mode == "duplicate-result" {
		write(result)
		return
	}
	lifecycle("completed")
}

type firstExchangeFixture struct {
	opts     Options
	recorder string
}

func newFirstExchangeFixture(t *testing.T, mode string) firstExchangeFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "work")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(root, "wire.jsonl")
	return firstExchangeFixture{Options{Executable: exe, Cwd: cwd, Model: "sonnet", ConfigArgs: []string{"--bare"}, Env: []string{"HOME=" + root, "TMPDIR=" + root, "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "CXT_FIRST_EXCHANGE_UNIT_HELPER=1", "CXT_FIRST_EXCHANGE_UNIT_MODE=" + mode, "CXT_FIRST_EXCHANGE_UNIT_RECORD=" + recorder, "GORACE=atexit_sleep_ms=0"}}, recorder}
}

func (f firstExchangeFixture) start(t *testing.T, reference bool) *FirstExchange {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	t.Cleanup(cancel)
	s, err := StartFirstExchange(ctx, f.opts)
	if err != nil {
		t.Fatal("synthetic start:", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if reference {
		r, err := s.AppendReference(ctx, "PRIVATE_SYNTHETIC_REFERENCE")
		if err != nil || !r.NoTurnAcknowledged || r.Persisted || r.ProviderAcceptance != "unverified" {
			t.Fatal("reference was not acknowledged without a turn:", err)
		}
	}
	return s
}

func (f firstExchangeFixture) frames(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(f.recorder)
	if err != nil {
		t.Fatal(err)
	}
	var frames []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		m, err := object([]byte(line))
		if err != nil {
			t.Fatal("invalid synthetic recorder")
		}
		frames = append(frames, m)
	}
	return frames
}

func (f firstExchangeFixture) queries(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	var queries []map[string]json.RawMessage
	for _, m := range f.frames(t) {
		if string(m["type"]) == `"control_response"` {
			t.Fatal("adapter automatically answered a permission request")
		}
		if string(m["type"]) == `"user"` && string(m["shouldQuery"]) == "true" {
			queries = append(queries, m)
		}
	}
	return queries
}

func firstExchangeRunContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func firstExchangeAllow(context.Context, FirstQuestionEvidence) error { return nil }

func TestFirstExchangeLiteralQuestionAndOneShot(t *testing.T) {
	f := newFirstExchangeFixture(t, "normal")
	s := f.start(t, true)
	const question = "/literal @file \u97d3\u6587\nPRIVATE_QUESTION"
	var admitted int
	r, err := s.Run(firstExchangeRunContext(t), question, func(ctx context.Context, e FirstQuestionEvidence) error {
		admitted++
		if _, ok := ctx.Deadline(); !ok || e.QuestionHash != hashText(question) || e.QuestionBytes != len(question) || e.Summary.SessionID != s.SessionID() || e.Summary.Model != "fixture-model" || e.Summary.TotalTokens != 57 || e.Summary.Measurement != "local_estimate" || !e.Reference.NoTurnAcknowledged || e.Reference.SessionID != s.SessionID() || e.Reference.Persisted || e.Reference.ProviderAcceptance != "unverified" {
			t.Error("incorrect admission evidence")
		}
		if len(f.queries(t)) != 0 {
			t.Error("question sent before admission returned")
		}
		return nil
	})
	if err != nil || !r.Completed || r.Answer != firstExchangeAnswer || r.SessionID != s.SessionID() || r.QuestionHash != hashText(question) || r.MessageID == "" || admitted != 1 || s.HostVersion() != "2.1.287" {
		t.Fatal("first text exchange failed:", err)
	}
	if _, err := s.Run(firstExchangeRunContext(t), question, firstExchangeAllow); err == nil {
		t.Fatal("second query allowed")
	}
	if err := s.Close(); err != nil {
		t.Fatal("orderly close:", err)
	}
	queries := f.queries(t)
	if len(queries) != 1 {
		t.Fatalf("query count = %d, want 1", len(queries))
	}
	q := queries[0]
	text, err := referenceText(q["message"])
	_, hasSynthetic := q["isSynthetic"]
	if err != nil || text != question || hasSynthetic || string(q["client_composed"]) != "true" || string(q["parent_tool_use_id"]) != "null" || string(q["session_id"]) != fmt.Sprintf("%q", s.SessionID()) || string(q["uuid"]) != fmt.Sprintf("%q", r.MessageID) {
		t.Fatal("query was transformed or had incorrect identity/flags")
	}
	var order []string
	for _, m := range f.frames(t) {
		if string(m["type"]) == `"user"` {
			order = append(order, "user:"+string(m["shouldQuery"]))
		} else if request, err := object(m["request"]); err == nil {
			method, _ := stringField(request, "subtype")
			order = append(order, method)
		}
	}
	if strings.Join(order, ",") != "initialize,user:false,get_context_usage,get_context_usage,user:true" {
		t.Fatalf("unexpected admission order: %v", order)
	}
}

func TestFirstExchangeAdmissionNeverWritesOnFailure(t *testing.T) {
	for _, mode := range []string{"no-reference", "nil-admit", "no-deadline", "admit-error", "canceled", "cancel-in-admit", "deadline-in-admit", "bad-reference"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, mode != "no-reference" && mode != "bad-reference")
			if mode == "bad-reference" {
				if _, err := s.AppendReference(firstExchangeRunContext(t), "PRIVATE_REFERENCE"); err == nil {
					t.Fatal("malformed reference acknowledgment accepted")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			admit := firstExchangeAllow
			var wantContext error
			switch mode {
			case "nil-admit":
				admit = nil
			case "no-deadline":
				ctx = context.Background()
			case "admit-error":
				admit = func(context.Context, FirstQuestionEvidence) error { return errors.New("PRIVATE_CALLBACK_ERROR") }
			case "canceled":
				cancel()
				wantContext = context.Canceled
			case "cancel-in-admit":
				admit = func(context.Context, FirstQuestionEvidence) error { cancel(); return nil }
				wantContext = context.Canceled
			case "deadline-in-admit":
				ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
				defer cancel()
				admit = func(ctx context.Context, _ FirstQuestionEvidence) error { <-ctx.Done(); return ctx.Err() }
				wantContext = context.DeadlineExceeded
			}
			r, err := s.Run(ctx, "PRIVATE_QUESTION", admit)
			if err == nil || r.Completed || r.Answer != "" || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
				t.Fatal("failed admission released a response or leaked callback data")
			}
			if wantContext != nil && !errors.Is(err, wantContext) {
				t.Fatal("context cancellation was lost:", err)
			}
			if mode == "admit-error" {
				if _, err := s.Run(firstExchangeRunContext(t), "retry", firstExchangeAllow); err == nil {
					t.Fatal("failed admission did not consume one-shot Run")
				}
			}
			_ = s.Close()
			if got := len(f.queries(t)); got != 0 {
				t.Fatalf("failed admission wrote %d queries", got)
			}
		})
	}
}

func TestFirstExchangeSummaryAndLaunchDrift(t *testing.T) {
	for _, mode := range []string{"summary-total-drift", "summary-model-drift", "summary-window-drift", "summary-threshold-drift", "summary-used", "launch-before", "launch-after"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, true)
			replaceCwd := func() {
				if err := os.Rename(f.opts.Cwd, f.opts.Cwd+"-retired"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.opts.Cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "launch-before" {
				replaceCwd()
			}
			r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", func(context.Context, FirstQuestionEvidence) error {
				if mode == "launch-after" {
					replaceCwd()
				}
				return nil
			})
			_ = s.Close()
			if err == nil || r.Completed || len(f.queries(t)) != 0 {
				t.Fatal("changed admission evidence released a query:", err)
			}
		})
	}
}

func TestFirstExchangeConcurrentRunConsumesOneAttempt(t *testing.T) {
	f := newFirstExchangeFixture(t, "normal")
	s := f.start(t, true)
	ctx := firstExchangeRunContext(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	type outcome struct {
		result FirstExchangeResult
		err    error
	}
	out := make(chan outcome, 2)
	admit := func(ctx context.Context, _ FirstQuestionEvidence) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	run := func() { r, err := s.Run(ctx, "same literal question", admit); out <- outcome{r, err} }
	go run()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first admission did not start")
	}
	// A shallow copy must share the reservation, and a losing call must not
	// close the transport while its owner's admission callback is in flight.
	copied := *s
	go func() {
		r, err := copied.Run(ctx, "same literal question", admit)
		out <- outcome{r, err}
	}()
	select {
	case o := <-out:
		if o.err == nil || o.result.Completed {
			t.Fatal("copied handle acquired an already reserved attempt")
		}
		out <- o
	case <-ctx.Done():
		t.Fatal("copied handle blocked behind the owner instead of rejecting")
	}
	close(release)
	successes, failures := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case o := <-out:
			if o.err == nil && o.result.Completed {
				successes++
			} else if o.err != nil && !o.result.Completed {
				failures++
			}
		case <-ctx.Done():
			t.Fatal("concurrent Run did not finish")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || failures != 1 || calls.Load() != 1 || len(f.queries(t)) != 1 {
		t.Fatal("concurrent calls did not preserve one-shot ownership")
	}
}

func TestFirstExchangeRejectsMalformedOrForeignProtocol(t *testing.T) {
	modes := []string{"foreign-lifecycle", "started-before-queued", "duplicate-queued", "missing-started", "completed-before-result", "foreign-echo", "changed-echo", "echo-parent", "missing-echo", "duplicate-echo", "permission", "unknown-control", "unknown-event", "malformed-json", "tool-use", "foreign-session", "foreign-result", "foreign-result-list", "error-result", "zero-turns", "multiple-turns", "empty-answer", "missing-result-usage", "negative-input", "null-input", "string-output", "zero-output", "foreign-model", "queued-turn", "denied-permission", "duplicate-result"}
	modes = append(modes, "assistant-query-missing", "assistant-query-foreign", "assistant-query-null", "sibling-query-foreign", "sibling-query-null", "result-index-missing", "result-index-reused", "result-index-skipped", "result-index-null", "result-index-string", "stop-reason-missing", "stop-reason-wrong")
	modes = append(modes, "assistant-message-id-missing", "assistant-message-id-empty", "sibling-message-id-foreign", "sibling-message-id-missing", "status-compacting", "terminal-reason-missing", "terminal-reason-null", "terminal-hook-stopped", "terminal-tool-deferred", "terminal-max-turns", "terminal-background-requested")
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, true)
			r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow)
			want := ErrProtocol
			if mode == "permission" || mode == "unknown-control" {
				want = ErrPermissionRequired
			}
			if !errors.Is(err, want) || r.Completed || r.Answer != "" || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
				t.Fatal("unsafe protocol accepted, wrong error, or private diagnostic:", err)
			}
			_ = s.Close()
			if len(f.queries(t)) != 1 {
				t.Fatal("protocol failure retried or query not recorded")
			}
		})
	}
}

func TestFirstExchangeReferenceIndexRequired(t *testing.T) {
	for _, mode := range []string{"reference-index-missing", "reference-index-wrong", "reference-index-null", "reference-index-string"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, false)
			if _, err := s.AppendReference(firstExchangeRunContext(t), "PRIVATE_REFERENCE"); !errors.Is(err, ErrProtocol) {
				t.Fatal("uncorrelated no-turn result index accepted:", err)
			}
			if r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow); err == nil || r.Completed {
				t.Fatal("question admitted after invalid reference result")
			}
			_ = s.Close()
			if len(f.queries(t)) != 0 {
				t.Fatal("invalid reference result index allowed a query")
			}
		})
	}
}

func TestFirstExchangeAssistantSiblingCorrelation(t *testing.T) {
	for _, mode := range []string{"sibling-query-omitted", "sibling-query-matched", "two-text-blocks"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, true)
			r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow)
			if err != nil || !r.Completed || r.Answer != firstExchangeAnswer || len(f.queries(t)) != 1 {
				t.Fatal("correlated visible text projection failed:", err)
			}
		})
	}
}

func TestFirstExchangeAllowsActiveRequestingStatus(t *testing.T) {
	f := newFirstExchangeFixture(t, "status-requesting-and-null")
	s := f.start(t, true)
	r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow)
	if err != nil || !r.Completed || r.Answer != firstExchangeAnswer || len(f.queries(t)) != 1 {
		t.Fatal("ordinary active-query status rejected:", err)
	}
}

func TestFirstExchangeCancellationAfterQuery(t *testing.T) {
	f := newFirstExchangeFixture(t, "silent-query")
	s := f.start(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r, err := s.Run(ctx, "PRIVATE_QUESTION", firstExchangeAllow)
	if !errors.Is(err, context.DeadlineExceeded) || r.Completed || r.Answer != "" {
		t.Fatal("incomplete exchange did not preserve cancellation:", err)
	}
	if _, err := s.Run(firstExchangeRunContext(t), "retry", firstExchangeAllow); err == nil {
		t.Fatal("uncertain query retried")
	}
	_ = s.Close()
	if len(f.queries(t)) != 1 {
		t.Fatal("canceled exchange was not exactly one query")
	}
}

func TestFirstExchangeAuditsLateOutputAtClose(t *testing.T) {
	for _, mode := range []string{"late-assistant", "late-compaction", "late-truncated"} {
		t.Run(mode, func(t *testing.T) {
			f := newFirstExchangeFixture(t, mode)
			s := f.start(t, true)
			r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow)
			if !errors.Is(err, ErrProtocol) || r.Completed || r.Answer != "" || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
				t.Fatal("late protocol output was ignored or leaked:", err)
			}
			if !errors.Is(s.Close(), ErrProtocol) {
				t.Fatal("repeated Close lost the EOF audit failure")
			}
			if len(f.queries(t)) != 1 {
				t.Fatal("late output caused a retry")
			}
		})
	}
}

func TestFirstExchangeVersionIsolation(t *testing.T) {
	ctx := firstExchangeRunContext(t)
	old := newFirstExchangeFixture(t, "old-version")
	if s, err := StartFirstExchange(ctx, old.opts); err == nil {
		_ = s.Close()
		t.Fatal("first exchange silently accepted no-query version")
	}
	// The same helper enforces --permission-prompts none for the unchanged
	// 2.1.285 no-query path, and host for each first-exchange process above.
	s, err := Start(ctx, old.opts)
	if err != nil {
		t.Fatal("legacy no-query launch options changed:", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	newer := newFirstExchangeFixture(t, "normal")
	if s, err := Start(ctx, newer.opts); err == nil {
		_ = s.Close()
		t.Fatal("default no-query Start broadened to 2.1.287")
	}
}

func TestFirstExchangeHostNotificationsAcrossLifetime(t *testing.T) {
	f := newFirstExchangeFixture(t, "host-notifications")
	s := f.start(t, true)
	r, err := s.Run(firstExchangeRunContext(t), "PRIVATE_QUESTION", firstExchangeAllow)
	if err != nil || !r.Completed || r.Answer != firstExchangeAnswer || len(f.queries(t)) != 1 {
		t.Fatal("notification lifetime failed:", err)
	}
}
