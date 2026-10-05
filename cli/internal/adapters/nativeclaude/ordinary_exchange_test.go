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

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func init() {
	if os.Getenv("CXT_ORDINARY_UNIT_HELPER") == "1" {
		time.AfterFunc(8*time.Second, func() { os.Exit(98) })
		os.Exit(ordinaryUnitHelper())
	}
}

func ordinaryFixture(t *testing.T, mode string) firstExchangeFixture {
	t.Helper()
	f := newFirstExchangeFixture(t, mode)
	for i, v := range f.opts.Env {
		if v == "CXT_FIRST_EXCHANGE_UNIT_HELPER=1" {
			f.opts.Env[i] = "CXT_ORDINARY_UNIT_HELPER=1"
		}
	}
	return f
}

// An actual owned helper process, never Claude. Its recorder, archive and
// environment are fresh synthetic state. EOF terminates it; the timer is a
// finite backstop. No network, tools, shell commands or descendants are used.
func ordinaryUnitHelper() int {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			if path := os.Getenv("CXT_ORDINARY_VERSION_ARCHIVE"); path != "" {
				_ = os.MkdirAll(filepath.Dir(path), 0700)
				_ = os.WriteFile(path, []byte("synthetic preexisting archive\n"), 0600)
			}
			fmt.Println("2.1.287 (Claude Code)")
			return 0
		}
	}
	var sid string
	for i, arg := range os.Args {
		if arg == "--session-id" && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
	}
	if bound := os.Getenv("CXT_WRAPPED_SESSION_ID"); bound != "" && bound != sid {
		return 90
	}
	mode := os.Getenv("CXT_FIRST_EXCHANGE_UNIT_MODE")
	f, err := os.OpenFile(os.Getenv("CXT_FIRST_EXCHANGE_UNIT_RECORD"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 91
	}
	defer f.Close()
	cwd, _ := os.Getwd()
	var rows []map[string]any
	parent := ""
	archive := func(kind, id, p string, message any) {
		if kind == "assistant" && mode == "finalized-metadata" {
			wire := message.(map[string]any)
			stored := map[string]any{}
			for key, value := range wire {
				stored[key] = value
			}
			delete(stored, "context_management")
			delete(stored, "container")
			delete(stored, "diagnostics")
			delete(stored, "input_transformations")
			stored["stop_details"] = nil
			stored["stop_reason"] = "end_turn"
			if id == "assistant-1" {
				stored["stop_reason"] = "tool_use"
			}
			stored["usage"] = map[string]any{"input_tokens": 64, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 4, "output_tokens_details": map[string]any{"thinking_tokens": 0}, "server_tool_use": map[string]any{"web_search_requests": 0, "web_fetch_requests": 0}, "service_tier": "standard", "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0}, "inference_geo": "", "iterations": []any{}, "speed": "standard", "fallback_credit": nil}
			message = stored
		}
		var pp any
		if p != "" {
			pp = p
		}
		row := map[string]any{"type": kind, "uuid": id, "parentUuid": pp, "sessionId": sid, "cwd": cwd, "isSidechain": false, "message": message}
		if kind == "assistant" {
			// The fixture has one native block per assistant call, except its
			// explicit parallel-tool batch (one fallback scalar for the batch).
			row["apiBlockIndex"] = 0
			if id == "assistant-1b" {
				row["apiBlockIndex"] = 1
			}
		}
		if len(rows) == 0 {
			row["isMeta"] = true
		}
		if kind == "user" && strings.HasPrefix(id, "tool-result-") {
			row["sourceToolAssistantUUID"] = p
		}
		rows = append(rows, row)
		parent = id
	}
	persist := func() {
		path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", providerfs.EncodeCwd(cwd), sid+".jsonl")
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		for _, r := range rows {
			_ = json.NewEncoder(file).Encode(r)
		}
	}
	write := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	hooks := func() {
		for _, subtype := range []string{"hook_started", "hook_progress", "hook_response"} {
			write(map[string]any{"type": "system", "subtype": subtype, "session_id": sid, "hook_id": "synthetic-hook", "hook_name": "UserPromptSubmit", "output": "PRIVATE_HOOK_DIAGNOSTIC"})
		}
	}
	lifecycle := func(id, state string) {
		write(map[string]any{"type": "command_lifecycle", "session_id": sid, "command_uuid": id, "state": state})
	}
	qid, name := "", "Read"
	var toolInput any = map[string]any{"file_path": "PRIVATE_SYNTHETIC_PATH"}
	if mode == "ask" || mode == "blocked-reply" {
		name = "AskUserQuestion"
		toolInput = map[string]any{"questions": []any{map[string]any{"question": "Choose?", "header": "Choice", "multiSelect": true, "options": []any{map[string]any{"label": "One", "description": "first"}, map[string]any{"label": "Two", "description": "second"}}}}}
		if mode == "blocked-reply" {
			var questions []any
			for i := 0; i < 4; i++ {
				var options []any
				for j := 0; j < 4; j++ {
					options = append(options, map[string]any{"label": fmt.Sprint(j), "preview": strings.Repeat("x", 8192)})
				}
				questions = append(questions, map[string]any{"question": fmt.Sprint(i), "header": "Choice", "options": options})
			}
			toolInput = map[string]any{"questions": questions}
		}
	}
	assistant := func(id, mid string, blocks []any) {
		message := map[string]any{"role": "assistant", "model": "fixture-model", "id": mid, "content": blocks}
		if mode == "finalized-metadata" {
			message["type"], message["stop_reason"], message["stop_sequence"], message["context_management"] = "message", nil, nil, nil
			message["usage"] = map[string]any{"input_tokens": 64, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 1}
			message["container"], message["diagnostics"], message["input_transformations"] = nil, nil, []any{}
		}
		archive("assistant", id, parent, message)
		write(map[string]any{"type": "assistant", "session_id": sid, "parent_tool_use_id": nil, "uuid": id, "user_message_uuid": qid, "message": message})
	}
	resultTool := func(id string) {
		message := map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "Synthetic tool result"}}}
		archive("user", "tool-result-"+id, "assistant-1", message)
		write(map[string]any{"type": "user", "session_id": sid, "parent_tool_use_id": nil, "uuid": "tool-result-" + id, "message": message})
	}
	denied := false
	finish := func() {
		assistant("assistant-2", "api-message-2", []any{map[string]any{"type": "text", "text": "Synthetic final answer."}})
		r := zeroResultFixture(sid, qid)
		r["result_index"], r["num_turns"], r["result"], r["terminal_reason"], r["stop_reason"] = 1, 2, "Synthetic final answer.", "completed", "end_turn"
		r["usage"] = map[string]any{"input_tokens": 20, "output_tokens": 4, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		r["modelUsage"] = map[string]any{"fixture-model": map[string]any{}}
		if denied {
			r["permission_denials"] = []any{map[string]any{"tool_use_id": "tool-1", "tool_name": name, "tool_input": toolInput}}
		}
		if mode == "bad-denial" {
			r["permission_denials"] = []any{map[string]any{"tool_use_id": "foreign", "tool_name": name}}
		}
		persist()
		write(r)
		lifecycle(qid, "completed")
	}
	request := func(id, tool string) {
		r := map[string]any{"subtype": "can_use_tool", "tool_name": name, "input": toolInput, "tool_use_id": tool}
		if mode == "ask" {
			r["requires_user_interaction"] = true
		}
		if mode == "specialized" {
			r["requires_user_interaction"] = true
		}
		if mode == "foreign-tool" {
			r["tool_use_id"] = "foreign"
		}
		if mode == "unknown-control" {
			r["subtype"] = "request_user_dialog"
		}
		write(map[string]any{"type": "control_request", "request_id": id, "request": r})
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	replies, summaries := 0, 0
	for scanner.Scan() {
		_, _ = f.Write(append(append([]byte{}, scanner.Bytes()...), '\n'))
		m, err := object(scanner.Bytes())
		if err != nil {
			return 92
		}
		kind, _ := stringField(m, "type")
		switch kind {
		case "control_request":
			r, _ := object(m["request"])
			method, _ := stringField(r, "subtype")
			id, _ := stringField(m, "request_id")
			var value any
			switch method {
			case "initialize":
				value = map[string]any{"models": []any{map[string]any{"value": "sonnet", "resolvedModel": "fixture-model"}}}
				if mode == "hooks" {
					hooks()
				}
			case "get_context_usage":
				summaries++
				total := 57
				if mode == "summary-drift" && summaries > 1 {
					total++
				}
				value = map[string]any{"model": "fixture-model", "totalTokens": total, "maxTokens": 1000000, "rawMaxTokens": 1000000, "isAutoCompactEnabled": true, "autoCompactThreshold": 967000, "apiUsage": nil}
			default:
				return 93
			}
			write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": value}})
		case "user":
			id, _ := stringField(m, "uuid")
			query, _ := boolean(m, "shouldQuery")
			if !query {
				text, _ := referenceText(m["message"])
				archive("user", id, parent, map[string]any{"role": "user", "content": nativeReferencePrefix + text})
				lifecycle(id, "queued")
				lifecycle(id, "started")
				r := zeroResultFixture(sid, id)
				r["result_index"] = 0
				write(r)
				lifecycle(id, "completed")
				continue
			}
			qid = id
			lifecycle(id, "queued")
			lifecycle(id, "started")
			if mode == "thinking-progress" {
				write(map[string]any{"type": "system", "subtype": "thinking_tokens", "session_id": sid, "uuid": "progress-1", "user_message_uuid": id, "estimated_tokens": 50, "estimated_tokens_delta": 50})
			}
			if mode == "hooks" {
				hooks()
			}
			message := m["message"]
			if mode == "rewritten" {
				message = json.RawMessage(`{"role":"user","content":"Native hook rewrite"}`)
			}
			archive("user", id, parent, message)
			write(map[string]any{"type": "user", "uuid": id, "session_id": sid, "parent_tool_use_id": nil, "message": message})
			blocks := []any{map[string]any{"type": "tool_use", "id": "tool-1", "name": name, "input": toolInput}}
			if mode == "parallel" {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": "tool-2", "name": name, "input": toolInput})
			}
			if mode == "sibling-tool-parent" {
				assistant("assistant-1", "api-message-1", []any{map[string]any{"type": "text", "text": ""}})
				assistant("assistant-1b", "api-message-1", blocks)
			} else {
				assistant("assistant-1", "api-message-1", blocks)
			}
			if mode == "preapproved" || mode == "finalized-metadata" || mode == "hooks" || mode == "rewritten" || mode == "bad-denial" || mode == "late" {
				resultTool("tool-1")
				finish()
				continue
			}
			if mode == "orphan-result" {
				resultTool("foreign")
				continue
			}
			request("request-1", "tool-1")
			if mode == "blocked-reply" {
				// Read one byte, proving the reply has committed, but leave the
				// much larger response undrained. A native hook wins meanwhile.
				var first [1]byte
				if _, err := os.Stdin.Read(first[:]); err != nil {
					return 97
				}
				write(map[string]any{"type": "control_cancel_request", "request_id": "request-1"})
				resultTool("tool-1")
				finish()
				return 0
			}
			if mode == "duplicate-request" {
				request("request-1", "tool-1")
			}
			if mode == "parallel" {
				request("request-2", "tool-2")
			}
			if mode == "withdraw" {
				write(map[string]any{"type": "control_cancel_request", "request_id": "request-1"})
				resultTool("tool-1")
				finish()
			}
		case "control_response":
			// Claude 2.1.287 replays the exact committed reply on stdout before
			// emitting the tool result. It must not become another decision.
			write(m)
			r, _ := object(m["response"])
			payload, _ := object(r["response"])
			if _, ok := payload["updatedPermissions"]; ok {
				return 94
			}
			denied = string(payload["behavior"]) == `"deny"`
			tool, _ := stringField(payload, "toolUseID")
			if mode == "withdraw" {
				return 95
			}
			resultTool(tool)
			replies++
			if mode == "duplicate-result" {
				resultTool(tool)
				continue
			}
			if mode != "parallel" || replies == 2 {
				finish()
			}
		default:
			return 96
		}
	}
	if mode == "late" {
		write(map[string]any{"type": "assistant", "session_id": sid, "parent_tool_use_id": nil, "uuid": "late"})
	}
	if mode == "cancel-eof" {
		// Native may treat stdin EOF as a denied permission and continue.
		// Cancellation must terminate this process before reaching that path.
		_ = os.WriteFile(filepath.Join(cwd, "continued-after-eof"), []byte("continued"), 0600)
	}
	return 0
}

func ordinaryResponses(t *testing.T, f firstExchangeFixture) ([]map[string]json.RawMessage, int) {
	t.Helper()
	var replies []map[string]json.RawMessage
	queries := 0
	for _, m := range f.frames(t) {
		if string(m["type"]) == `"control_response"` {
			r, _ := object(m["response"])
			p, _ := object(r["response"])
			replies = append(replies, p)
		}
		if string(m["type"]) == `"user"` && string(m["shouldQuery"]) == "true" {
			queries++
			if _, ok := m["isSynthetic"]; ok {
				t.Fatal("ordinary input marked synthetic")
			}
			if _, ok := m["client_composed"]; ok {
				t.Fatal("ordinary preprocessing skipped")
			}
		}
	}
	return replies, queries
}

func TestOrdinaryExchangeToolsAndArchive(t *testing.T) {
	for _, mode := range []string{"normal", "thinking-progress", "preapproved", "finalized-metadata", "hooks", "parallel", "sibling-tool-parent", "duplicate-request", "deny", "rewritten", "ask"} {
		t.Run(mode, func(t *testing.T) {
			f := ordinaryFixture(t, mode)
			e := f.start(t, true)
			var calls atomic.Int32
			h := InteractionHandlers{CanUseTool: func(ctx context.Context, r ToolPermissionRequest) (ToolPermissionDecision, error) {
				calls.Add(1)
				// Mutating the UI copy cannot alter the granted native input.
				r.Input[0] = 'x'
				if mode == "deny" {
					return DenyOnce, nil
				}
				return AllowOnce, nil
			}, AskUserQuestion: func(ctx context.Context, r UserQuestionRequest) (UserQuestionAnswers, error) {
				calls.Add(1)
				r.Questions[0].Options[0].Label = "MUTATED"
				return UserQuestionAnswers{Answers: map[string][]string{"Choose?": {"One", "Other, custom"}}}, nil
			}}
			r, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, h)
			if err != nil || !r.Completed || r.Answer != "Synthetic final answer." {
				t.Fatal("ordinary completion", err)
			}
			replies, queries := ordinaryResponses(t, f)
			want := 1
			if mode == "parallel" {
				want = 2
			}
			if mode == "preapproved" || mode == "finalized-metadata" || mode == "hooks" || mode == "rewritten" {
				want = 0
			}
			if queries != 1 || len(replies) != want || int(calls.Load()) != want {
				t.Fatal("unexpected interactions", queries, len(replies), calls.Load())
			}
			for _, reply := range replies {
				if _, ok := reply["updatedPermissions"]; ok {
					t.Fatal("persistent permission update")
				}
			}
			if mode == "ask" {
				input, _ := object(replies[0]["updatedInput"])
				if strings.Contains(string(input["questions"]), "MUTATED") {
					t.Fatal("callback changed shown questions")
				}
				if string(input["answers"]) != `{"Choose?":["One","Other, custom"]}` {
					t.Fatal("answers changed")
				}
			}
			proof, err := e.VerifyArchive(context.Background(), unitArchive(e.s))
			wantAssistants := 2
			if mode == "sibling-tool-parent" {
				wantAssistants = 3
			}
			if err != nil || !proof.Persisted || proof.AssistantRecords != wantAssistants || proof.ToolResultRecords < 1 || proof.QuestionHash != hashText("ordinary question") {
				t.Fatal("archive", err)
			}
			if mode == "rewritten" && proof.NativeQuestionHash == proof.QuestionHash {
				t.Fatal("rewritten native input mislabeled")
			}
			if _, err := e.PrepareIdleResume(context.Background(), unitArchive(e.s)); err != nil {
				t.Fatal("verified resume plan", err)
			}
		})
	}
}

func TestOrdinaryExchangeRefusesBeforeQuestion(t *testing.T) {
	for _, mode := range []string{"missing-admit", "failed-admit", "slash", "summary-drift", "cancel", "config-drift"} {
		t.Run(mode, func(t *testing.T) {
			f := ordinaryFixture(t, mode)
			e := f.start(t, true)
			question := "ordinary question"
			admit := firstExchangeAllow
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			switch mode {
			case "missing-admit":
				admit = nil
			case "failed-admit":
				admit = func(context.Context, FirstQuestionEvidence) error { return errors.New("PRIVATE_CALLBACK") }
			case "slash":
				question = " /help"
			case "cancel":
				cancel()
			case "config-drift":
				admit = func(context.Context, FirstQuestionEvidence) error {
					if err := os.Rename(f.opts.Cwd, f.opts.Cwd+"-retired"); err != nil {
						return err
					}
					return os.Mkdir(f.opts.Cwd, 0700)
				}
			}
			_, err := e.RunOrdinary(ctx, question, admit, InteractionHandlers{})
			if err == nil || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE_CALLBACK") {
				t.Fatal("admission failure", err)
			}
			_, queries := ordinaryResponses(t, f)
			if queries != 0 {
				t.Fatal("unadmitted question sent")
			}
		})
	}
}

func TestOrdinaryExchangeWithdrawDoesNotCloseTurn(t *testing.T) {
	f := ordinaryFixture(t, "withdraw")
	e := f.start(t, true)
	returned := make(chan struct{})
	h := InteractionHandlers{CanUseTool: func(ctx context.Context, _ ToolPermissionRequest) (ToolPermissionDecision, error) {
		<-ctx.Done()
		close(returned)
		return AllowOnce, nil
	}}
	r, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, h)
	if err != nil || !r.Completed {
		t.Fatal("withdraw killed turn", err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("callback not cancelled")
	}
	replies, _ := ordinaryResponses(t, f)
	if len(replies) != 0 {
		t.Fatal("late grant sent")
	}
}

func TestOrdinaryExchangeCancellationWhilePrompting(t *testing.T) {
	f := ordinaryFixture(t, "cancel-eof")
	e := f.start(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.RunOrdinary(ctx, "ordinary question", firstExchangeAllow, InteractionHandlers{CanUseTool: func(ctx context.Context, _ ToolPermissionRequest) (ToolPermissionDecision, error) {
			close(entered)
			<-ctx.Done()
			return AllowOnce, nil
		}})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("no interaction")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
		t.Fatal("cancel lost", err)
	}
	if _, err := os.Stat(filepath.Join(f.opts.Cwd, "continued-after-eof")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancellation was converted to EOF and continued the turn", err)
	}
	if err := e.Close(); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
		t.Fatal("retired cancellation did not retain its distinct failure", err)
	}
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
		t.Fatal("aborted exchange was authorized for readback")
	}
	replies, _ := ordinaryResponses(t, f)
	if len(replies) != 0 {
		t.Fatal("grant after cancel")
	}
	if e.s.process.cmd.ProcessState == nil {
		t.Fatal("child not reaped")
	}
}

func TestOrdinaryExchangeAuditsCommittedReplyAtShutdown(t *testing.T) {
	f := ordinaryFixture(t, "blocked-reply")
	e := f.start(t, true)
	r, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{
		AskUserQuestion: func(context.Context, UserQuestionRequest) (UserQuestionAnswers, error) {
			return UserQuestionAnswers{Answers: map[string][]string{}}, nil
		},
	})
	if err == nil || r.Completed {
		t.Fatal("completed before interrupted permission write was audited")
	}
	if e.s.process.cmd.ProcessState == nil {
		t.Fatal("child not reaped")
	}
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); !errors.Is(err, ErrState) {
		t.Fatal("write failure authorized archive", err)
	}
}

func TestOrdinaryExchangeCopiedHandleOneShot(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	copy := *e
	errs := make(chan error, 2)
	for _, handle := range []*FirstExchange{e, &copy} {
		go func(e *FirstExchange) {
			_, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{})
			errs <- err
		}(handle)
	}
	a, b := <-errs, <-errs
	if !((a == nil && errors.Is(b, ErrState)) || (b == nil && errors.Is(a, ErrState))) {
		t.Fatal("copied attempt", a, b)
	}
	_, queries := ordinaryResponses(t, f)
	if queries != 1 {
		t.Fatal("query duplicated")
	}
}

func TestOrdinaryExchangeRejectsProtocolAndInteractionFailures(t *testing.T) {
	for _, mode := range []string{"foreign-tool", "unknown-control", "specialized", "orphan-result", "duplicate-result", "bad-denial", "late", "missing-handler", "callback-error", "invalid-decision"} {
		t.Run(mode, func(t *testing.T) {
			f := ordinaryFixture(t, mode)
			e := f.start(t, true)
			privateErr := errors.New("PRIVATE_CALLBACK_FAILURE")
			h := InteractionHandlers{CanUseTool: func(context.Context, ToolPermissionRequest) (ToolPermissionDecision, error) {
				if mode == "callback-error" {
					return 0, privateErr
				}
				if mode == "invalid-decision" {
					return 0, nil
				}
				return AllowOnce, nil
			}}
			if mode == "missing-handler" {
				h = InteractionHandlers{}
			}
			_, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, h)
			if err == nil {
				t.Fatal("invalid ordinary turn completed")
			}
			if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE_CALLBACK") {
				t.Fatal("private diagnostic")
			}
			if mode == "callback-error" && !errors.Is(err, privateErr) {
				t.Fatal("callback cause lost", err)
			}
			if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); !errors.Is(err, ErrState) {
				t.Fatal("failed turn authorized archive", err)
			}
		})
	}
}

func TestOrdinaryArchiveRejectsGraphAndBehaviorMutation(t *testing.T) {
	for _, mode := range []string{"parent", "source", "content", "model", "message-behavior", "hidden", "unknown", "missing-result"} {
		t.Run(mode, func(t *testing.T) {
			f := ordinaryFixture(t, "preapproved")
			e := f.start(t, true)
			if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
				t.Fatal(err)
			}
			path := unitArchive(e.s)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var rows []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var row map[string]any
				if json.Unmarshal([]byte(line), &row) != nil {
					t.Fatal("fixture")
				}
				rows = append(rows, row)
			}
			switch mode {
			case "parent":
				rows[3]["parentUuid"] = rows[1]["uuid"]
			case "source":
				rows[3]["sourceToolAssistantUUID"] = rows[1]["uuid"]
			case "content":
				rows[3]["message"].(map[string]any)["content"] = []any{}
			case "model":
				rows[4]["message"].(map[string]any)["model"] = "other"
			case "message-behavior":
				rows[4]["message"].(map[string]any)["stop_reason"] = "tool_use"
			case "hidden":
				rows[3]["isMeta"] = true
			case "unknown":
				rows[3]["toolEndsTurn"] = true
			case "missing-result":
				rows = append(rows[:3], rows[4:]...)
			}
			writeExchangeRows(t, e, rows)
			if _, err := e.VerifyArchive(context.Background(), path); err == nil {
				t.Fatal("archive mutation accepted")
			}
		})
	}
}
