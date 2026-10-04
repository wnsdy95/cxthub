package nativecodex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Synthetic v2 fixtures, checked against the installed 0.157.1 schemas in
// /tmp/cxt-native-generation-schema. These tests require no native process,
// credentials, network, or model calls.
func observationFixture(t *testing.T, o *initialTurnObservation, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Observe(method, raw); err != nil {
		t.Fatalf("Observe(%s): %v", method, err)
	}
}

func observationParams(extra map[string]any) map[string]any {
	p := map[string]any{"threadId": "thread-1", "turnId": "turn-1"}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func observationTurn(status string, items ...any) map[string]any {
	if items == nil {
		items = []any{}
	}
	return map[string]any{"threadId": "thread-1", "turn": map[string]any{
		"id": "turn-1", "status": status, "items": items,
	}}
}

func observationBreakdown(input, cached, output int) map[string]any {
	return map[string]any{
		"inputTokens": input, "cachedInputTokens": cached, "outputTokens": output,
		"reasoningOutputTokens": 0, "totalTokens": input + output,
	}
}

func observationUsage(input int) map[string]any {
	return observationParams(map[string]any{"tokenUsage": map[string]any{
		"last":               observationBreakdown(input, input/2, 10),
		"total":              observationBreakdown(900000, 200000, 900), // deliberately cumulative
		"modelContextWindow": 1000000,
	}})
}

func observationOutput(t *testing.T, o *initialTurnObservation) {
	t.Helper()
	observationFixture(t, o, "item/agentMessage/delta", observationParams(map[string]any{
		"itemId": "message-1", "delta": "private synthetic output",
	}))
}

func observationItem(t *testing.T, o *initialTurnObservation, kind string, started bool) {
	t.Helper()
	method, timestamp := "item/completed", "completedAtMs"
	if started {
		method, timestamp = "item/started", "startedAtMs"
	}
	observationFixture(t, o, method, observationParams(map[string]any{
		"item": map[string]any{"id": "item-1", "type": kind}, timestamp: 42,
	}))
}

func observationError(code any, retry bool) map[string]any {
	return observationParams(map[string]any{
		"error":     map[string]any{"message": "private synthetic error", "codexErrorInfo": code},
		"willRetry": retry,
	})
}

func assertObservationUnknownUsage(t *testing.T, got GenerationObservation) {
	t.Helper()
	if got.UsageKnown || got.UsageBeforeCompaction || got.TotalInputTokens != 0 || got.ModelContextWindow != 0 {
		t.Fatalf("unexpected input usage: %+v", got)
	}
	if got.ExecutionKnown && !got.ExecutionStarted {
		t.Fatalf("v2 cannot prove non-execution: %+v", got)
	}
}

func TestInitialTurnObservationFirstUsageAndLaterRequests(t *testing.T) {
	for _, withStart := range []bool{false, true} {
		t.Run(fmt.Sprint("start-notification-", withStart), func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			if withStart {
				observationFixture(t, o, "turn/started", observationTurn("inProgress"))
			}
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(1200))
			first := o.Result()
			if !first.UsageKnown || !first.UsageBeforeCompaction || first.TotalInputTokens != 1200 || first.ModelContextWindow != 1000000 || !first.ExecutionKnown || !first.ExecutionStarted {
				t.Fatalf("first request: %+v", first)
			}
			// A later tool request and usage must not replace the initial request.
			observationItem(t, o, "commandExecution", true)
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(4700))
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(4700))
			observationFixture(t, o, "turn/completed", observationTurn("completed"))
			got := o.Result()
			first.Outcome = "completed"
			if got != first {
				t.Fatalf("first usage changed: got %+v, want %+v", got, first)
			}
		})
	}
}

func TestInitialTurnObservationAcknowledgementAndCompletionAreNotExecution(t *testing.T) {
	o := newInitialTurnObservation("thread-1", "turn-1")
	want := GenerationObservation{ThreadID: "thread-1", TurnID: "turn-1", Outcome: "unknown"}
	observationFixture(t, o, "turn/start", map[string]any{"turn": map[string]any{"id": "turn-1"}})
	observationFixture(t, o, "turn/started", observationTurn("inProgress"))
	observationItem(t, o, "userMessage", true)
	observationItem(t, o, "hookPrompt", false)
	observationFixture(t, o, "item/agentMessage/delta", observationParams(map[string]any{"itemId": "message-1", "delta": ""}))
	if got := o.Result(); got != want {
		t.Fatalf("ACK/input marked execution: %+v", got)
	}
	observationFixture(t, o, "turn/completed", observationTurn("completed"))
	want.Outcome = "completed"
	if got := o.Result(); got != want {
		t.Fatalf("empty completion marked execution: %+v", got)
	}
	// The terminal result is stable; a late usage cannot create a first sample.
	observationOutput(t, o)
	observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(2300))
	if got := o.Result(); got != want {
		t.Fatalf("late notification changed result: %+v", got)
	}
}

func TestInitialTurnObservationAmbiguousFirstRequestRemainsUnknown(t *testing.T) {
	for _, boundary := range []string{"unanchored-usage", "zero-output-usage", "tool", "retry", "unknown-item", "replayed-start"} {
		t.Run(boundary, func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			observationFixture(t, o, "turn/started", observationTurn("inProgress"))
			switch boundary {
			case "unanchored-usage":
				observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(800))
			case "zero-output-usage":
				observationOutput(t, o)
				p := observationUsage(800)
				p["tokenUsage"].(map[string]any)["last"] = observationBreakdown(800, 0, 0)
				observationFixture(t, o, "thread/tokenUsage/updated", p)
			case "tool":
				observationItem(t, o, "dynamicToolCall", true)
			case "retry":
				observationFixture(t, o, "error", observationError("rateLimitExceeded", true))
			case "unknown-item":
				observationItem(t, o, "futureRequestBoundary", true)
			case "replayed-start":
				observationFixture(t, o, "turn/started", observationTurn("inProgress"))
			}
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(2200))
			observationFixture(t, o, "turn/completed", observationTurn("completed"))
			assertObservationUnknownUsage(t, o.Result())
		})
	}
}

func TestInitialTurnObservationExactCorrelation(t *testing.T) {
	for _, method := range []string{"thread/tokenUsage/updated", "model/rerouted", "thread/compacted", "error", "item/started", "item/agentMessage/delta", "turn/started", "turn/completed"} {
		for _, foreign := range []string{"threadId", "turnId"} {
			t.Run(method+"/"+foreign, func(t *testing.T) {
				o := newInitialTurnObservation("thread-1", "turn-1")
				before := o.Result()
				// Foreign bodies need not be decoded; they must never influence
				// this turn, even when their usage or error body is malformed.
				p := observationParams(map[string]any{foreign: "foreign"})
				if strings.HasPrefix(method, "turn/") {
					p = observationTurn("completed")
					if foreign == "threadId" {
						p["threadId"] = "foreign"
					} else {
						p["turn"].(map[string]any)["id"] = "foreign"
					}
				}
				observationFixture(t, o, method, p)
				if got := o.Result(); got != before {
					t.Fatalf("foreign event changed observation: %+v", got)
				}
			})
		}
	}
}

func TestInitialTurnObservationRerouteInvalidatesScope(t *testing.T) {
	for _, timing := range []string{"before", "after-usage", "after-completed"} {
		t.Run(timing, func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			if timing != "before" {
				observationOutput(t, o)
				observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(1000))
			}
			if timing == "after-completed" {
				observationFixture(t, o, "turn/completed", observationTurn("completed"))
			}
			observationFixture(t, o, "model/rerouted", observationParams(map[string]any{
				"fromModel": "prepared-model", "toModel": "other-model", "reason": "highRiskCyberActivity",
			}))
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(9999))
			observationFixture(t, o, "turn/completed", observationTurn("completed"))
			got := o.Result()
			assertObservationUnknownUsage(t, got)
			if got.Outcome != "unknown" || !got.ModelRerouted || !got.Ineligible {
				t.Fatalf("reroute accepted: %+v", got)
			}
		})
	}
}

func TestInitialTurnObservationCompaction(t *testing.T) {
	t.Run("initial", func(t *testing.T) {
		o := newInitialTurnObservation("thread-1", "turn-1")
		observationItem(t, o, "contextCompaction", true)
		observationOutput(t, o)
		observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(900))
		observationItem(t, o, "contextCompaction", false)
		observationFixture(t, o, "thread/compacted", observationParams(nil))
		observationFixture(t, o, "turn/completed", observationTurn("completed"))
		assertObservationUnknownUsage(t, o.Result())
		if o.Result().Outcome != "initial_compaction" {
			t.Fatalf("lost initial compaction: %+v", o.Result())
		}
	})
	for _, sawStart := range []bool{false, true} {
		t.Run(fmt.Sprint("after-usage-start-", sawStart), func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(1200))
			if sawStart {
				observationItem(t, o, "contextCompaction", true)
			}
			observationItem(t, o, "contextCompaction", false)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(50))
			observationFixture(t, o, "turn/completed", observationTurn("completed"))
			got := o.Result()
			if got.Outcome != "completed" {
				t.Fatalf("late compaction classified as initial: %+v", got)
			}
			if sawStart {
				if !got.UsageKnown || !got.UsageBeforeCompaction || got.TotalInputTokens != 1200 {
					t.Fatalf("lost known pre-compaction usage: %+v", got)
				}
			} else {
				assertObservationUnknownUsage(t, got)
			}
		})
	}
}

func TestInitialTurnObservationInputRejectionNeverAttestsNonexecution(t *testing.T) {
	for _, executed := range []bool{false, true} {
		t.Run(fmt.Sprint("execution-", executed), func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			if executed {
				observationItem(t, o, "commandExecution", true)
			}
			p := observationError("contextWindowExceeded", false)
			p["executionStarted"] = false // unrecognized flags are not attestations
			p["executionKnown"] = true
			observationFixture(t, o, "error", p)
			terminal := observationTurn("failed")
			terminal["turn"].(map[string]any)["error"] = p["error"]
			observationFixture(t, o, "turn/completed", terminal)
			got := o.Result()
			want := "input_rejected"
			if executed {
				want = "unknown"
			}
			if got.Outcome != want || got.ExecutionKnown != executed || got.ExecutionStarted != executed {
				t.Fatalf("unsafe rejection inference: %+v", got)
			}
			assertObservationUnknownUsage(t, got)
		})
	}
	for _, code := range []any{nil, "badRequest", "ContextWindowExceeded", map[string]any{"responseStreamDisconnected": map[string]any{}}} {
		o := newInitialTurnObservation("thread-1", "turn-1")
		p := observationError(code, false)
		p["error"].(map[string]any)["message"] = "ContextWindowExceeded: input too long"
		observationFixture(t, o, "error", p)
		if got := o.Result(); got.Outcome != "unknown" || got.ExecutionKnown {
			t.Fatalf("message/unrelated error attested nonexecution: %+v", got)
		}
	}
}

func TestInitialTurnObservationInterruptedOrMissingTerminal(t *testing.T) {
	for _, status := range []string{"", "interrupted", "failed"} {
		o := newInitialTurnObservation("thread-1", "turn-1")
		observationFixture(t, o, "turn/started", observationTurn("inProgress"))
		if status != "" {
			observationFixture(t, o, "turn/completed", observationTurn(status))
		}
		got := o.Result() // transport timeout/disconnect supplies no new evidence
		if got.Outcome != "unknown" || got.ExecutionKnown || got.ExecutionStarted {
			t.Fatalf("absence/interruption attested nonexecution: %+v", got)
		}
		assertObservationUnknownUsage(t, got)
	}
}

func TestInitialTurnObservationErrorUsageAttribution(t *testing.T) {
	for _, laterRequest := range []bool{false, true} {
		t.Run(fmt.Sprint("later-request-", laterRequest), func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(1300))
			if laterRequest {
				observationItem(t, o, "commandExecution", true)
			}
			terminal := observationTurn("failed", map[string]any{"id": "message-1", "type": "agentMessage"})
			terminal["turn"].(map[string]any)["error"] = observationError("contextWindowExceeded", false)["error"]
			observationFixture(t, o, "turn/completed", terminal)
			got := o.Result()
			if laterRequest {
				if !got.UsageKnown || got.TotalInputTokens != 1300 {
					t.Fatalf("later failure lost first usage: %+v", got)
				}
			} else {
				assertObservationUnknownUsage(t, got)
			}
			if got.Outcome != "unknown" || !got.ExecutionStarted {
				t.Fatalf("error after execution was input rejection: %+v", got)
			}
		})
	}
}

func TestInitialTurnObservationMalformedMetadataIsStickyAndSanitized(t *testing.T) {
	cases := []struct{ method, raw string }{
		{"thread/tokenUsage/updated", `null`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1"}`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":null}`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-1","turnId":"other"}`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","ThreadId":"other","turnId":"turn-1"}`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{}}`},
		{"thread/tokenUsage/updated", `{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":null,"total":null}}`},
		{"turn/started", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}`},
		{"turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}`},
		{"turn/completed", `{"threadId":"thread-1","turnId":"other","turn":{"id":"turn-1","status":"completed","items":[]}}`},
		{"turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[],"error":{"message":"secret"}}}`},
		{"item/started", `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"item-1","type":null}}`},
		{"item/agentMessage/delta", `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":null}`},
		{"error", `{"threadId":"thread-1","turnId":"turn-1","error":{"message":"secret"}}`},
		{"error", `{"threadId":"thread-1","turnId":"turn-1","willRetry":"false","error":{"message":"secret"}}`},
		{"error", `{"threadId":"thread-1","turnId":"turn-1","willRetry":false,"error":{"message":null}}`},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			o := newInitialTurnObservation("thread-1", "turn-1")
			observationOutput(t, o)
			observationFixture(t, o, "thread/tokenUsage/updated", observationUsage(1200))
			err := o.Observe(tc.method, json.RawMessage(tc.raw))
			if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("want sanitized protocol error, got %v", err)
			}
			assertObservationUnknownUsage(t, o.Result())
			if !o.Result().Ineligible || o.Result().Outcome != "unknown" {
				t.Fatalf("malformed observation remained eligible: %+v", o.Result())
			}
			if !errors.Is(o.Observe("thread/compacted", json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1"}`)), ErrProtocol) {
				t.Fatal("protocol failure was not sticky")
			}
		})
	}
}

func TestInitialTurnObservationDuplicateAndAliasedUsageFields(t *testing.T) {
	base, _ := json.Marshal(observationUsage(1000))
	for _, pair := range [][2]string{
		{`"inputTokens":1000`, `"inputTokens":1000,"inputTokens":2000`},
		{`"inputTokens":1000`, `"inputTokens":1000,"InputTokens":2000`},
		{`"inputTokens":1000`, `"inputTokens":1000,"CacheWriteInputTokens":1000`},
		{`"modelContextWindow":1000000`, `"modelContextWindow":1000000,"ModelContextWindow":2000000`},
		{`"tokenUsage":`, `"TokenUsage":`},
	} {
		o := newInitialTurnObservation("thread-1", "turn-1")
		observationOutput(t, o)
		raw := strings.Replace(string(base), pair[0], pair[1], 1)
		if !errors.Is(o.Observe("thread/tokenUsage/updated", json.RawMessage(raw)), ErrProtocol) {
			t.Fatalf("ambiguous usage accepted: %s", raw)
		}
		assertObservationUnknownUsage(t, o.Result())
	}
}

func TestInitialTurnObservationTypedNonnegativeCounts(t *testing.T) {
	for _, path := range []string{"last", "total"} {
		for _, field := range []string{"inputTokens", "cachedInputTokens", "outputTokens", "reasoningOutputTokens", "totalTokens", "cacheWriteInputTokens"} {
			for _, bad := range []any{nil, -1, "100", 1.5, json.RawMessage(`1e3`), json.RawMessage(`9223372036854775808`)} {
				t.Run(fmt.Sprint(path, "/", field, "/", bad), func(t *testing.T) {
					o := newInitialTurnObservation("thread-1", "turn-1")
					observationOutput(t, o)
					p := observationUsage(1000)
					p["tokenUsage"].(map[string]any)[path].(map[string]any)[field] = bad
					raw, _ := json.Marshal(p)
					if err := o.Observe("thread/tokenUsage/updated", raw); !errors.Is(err, ErrProtocol) {
						t.Fatalf("invalid count accepted: %v", err)
					}
					assertObservationUnknownUsage(t, o.Result())
				})
			}
		}
	}
	for _, field := range []string{"inputTokens", "cachedInputTokens", "outputTokens", "reasoningOutputTokens", "totalTokens"} {
		p := observationUsage(1000)
		delete(p["tokenUsage"].(map[string]any)["last"].(map[string]any), field)
		raw, _ := json.Marshal(p)
		o := newInitialTurnObservation("thread-1", "turn-1")
		if !errors.Is(o.Observe("thread/tokenUsage/updated", raw), ErrProtocol) {
			t.Fatalf("missing %s accepted", field)
		}
	}
	for _, field := range []string{"cachedInputTokens", "cacheWriteInputTokens", "reasoningOutputTokens"} {
		p := observationUsage(1000)
		p["tokenUsage"].(map[string]any)["last"].(map[string]any)[field] = 1001
		raw, _ := json.Marshal(p)
		o := newInitialTurnObservation("thread-1", "turn-1")
		if !errors.Is(o.Observe("thread/tokenUsage/updated", raw), ErrProtocol) {
			t.Fatalf("subset %s larger than its inclusive count accepted", field)
		}
	}
}

func TestInitialTurnObservationOptionalWindowAndCacheWrites(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		o := newInitialTurnObservation("thread-1", "turn-1")
		observationOutput(t, o)
		p := observationUsage(1000)
		delete(p["tokenUsage"].(map[string]any), "modelContextWindow")
		observationFixture(t, o, "thread/tokenUsage/updated", p)
		if got := o.Result(); !got.UsageKnown || got.ModelContextWindow != 0 {
			t.Fatalf("missing window inferred capacity: %+v", got)
		}
	})
	for _, window := range []any{nil, 0, 1000000} {
		o := newInitialTurnObservation("thread-1", "turn-1")
		observationOutput(t, o)
		p := observationUsage(1000)
		u := p["tokenUsage"].(map[string]any)
		u["modelContextWindow"] = window
		u["last"].(map[string]any)["cacheWriteInputTokens"] = 300
		observationFixture(t, o, "thread/tokenUsage/updated", p)
		if got := o.Result(); !got.UsageKnown || got.TotalInputTokens != 1000 {
			t.Fatalf("cache writes added to inclusive input: %+v", got)
		}
	}
	for _, window := range []any{-1, "1000000", 1.2} {
		o := newInitialTurnObservation("thread-1", "turn-1")
		p := observationUsage(1000)
		p["tokenUsage"].(map[string]any)["modelContextWindow"] = window
		raw, _ := json.Marshal(p)
		if !errors.Is(o.Observe("thread/tokenUsage/updated", raw), ErrProtocol) {
			t.Fatal("invalid model window accepted")
		}
	}
}

func TestInitialTurnObservationBudgetsAndConcurrentSnapshots(t *testing.T) {
	o := newInitialTurnObservation("thread-1", "turn-1")
	var readers sync.WaitGroup
	readers.Go(func() {
		for range maxInitialObservationEvents {
			_ = o.Result()
		}
	})
	for range maxInitialObservationEvents {
		if err := o.Observe("unrelated", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	if !errors.Is(o.Observe("unrelated", json.RawMessage(`{}`)), ErrProtocol) {
		t.Fatal("event budget not enforced")
	}
	assertObservationUnknownUsage(t, o.Result())
	o = newInitialTurnObservation("thread-1", "turn-1")
	if !errors.Is(o.Observe("unrelated", json.RawMessage(strings.Repeat(" ", maxMessageBytes+1))), ErrProtocol) {
		t.Fatal("frame budget not enforced")
	}
	o = newInitialTurnObservation("thread-1", "turn-1")
	chunk := json.RawMessage(strings.Repeat(" ", maxMessageBytes-256))
	for range 2 {
		if err := o.Observe("unrelated", chunk); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(o.Observe("unrelated", chunk), ErrProtocol) {
		t.Fatal("aggregate byte budget not enforced")
	}
}

func TestInitialTurnObservationMetadataOnly(t *testing.T) {
	o := newInitialTurnObservation("thread-1", "turn-1")
	observationOutput(t, o)
	observationFixture(t, o, "error", observationError("badRequest", false))
	raw, err := json.Marshal(o.Result())
	if err != nil || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "synthetic") {
		t.Fatalf("result leaked a body: %s, %v", raw, err)
	}
	copy := o.Result()
	copy.TotalInputTokens = 123
	if o.Result().TotalInputTokens != 0 {
		t.Fatal("caller mutated collector state through Result")
	}
}
