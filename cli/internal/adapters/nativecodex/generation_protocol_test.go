package nativecodex

import (
	"encoding/json"
	"strings"
	"testing"
)

func generationUnit(t *testing.T) handoffUnitFixture {
	f := newHandoffUnitFixture(t)
	f.p.generation = newGenerationProtocol(f.p.thread)
	f.ready(t)
	return f
}
func generationStart(t *testing.T, f handoffUnitFixture, key string) {
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, key, "turn/start", map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "first question", "text_elements": []any{}}}}), true, false, false)
	f.p.generation.released = true
}
func generationNotice(t *testing.T, f handoffUnitFixture, method string, params any, reject bool) {
	handoffUnitObserve(t, f.p, handoffUnitJSON(t, map[string]any{"method": method, "params": params}), false, false, reject)
}
func generationTurn(status string) map[string]any {
	return map[string]any{"id": "turn-1", "status": status, "items": []any{}, "error": nil}
}

func TestGenerationRequiresResumeAndUnchangedSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"model", map[string]any{"model": "other"}}, {"cwd", map[string]any{"cwd": "/elsewhere"}}, {"permission", map[string]any{"approvalPolicy": "never"}},
		{"sandbox", map[string]any{"sandboxPolicy": map[string]any{"type": "dangerFullAccess"}}}, {"unknown", map[string]any{"injected": "extra"}},
		{"output", map[string]any{"toolOutput": []any{}}}, {"image", map[string]any{"input": []any{map[string]any{"type": "image", "url": "private"}}}},
		{"mixed", map[string]any{"input": []any{map[string]any{"type": "text", "text": "one"}, map[string]any{"type": "text", "text": "two"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := generationUnit(t)
			p := map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "q"}}}
			for k, v := range tc.extra {
				p[k] = v
			}
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "go", "turn/start", p), true, false, true)
		})
	}
	f := newHandoffUnitFixture(t)
	f.p.generation = newGenerationProtocol(f.p.thread)
	f.initialize(t)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "go", "turn/start", map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "q"}}}), true, false, true)
}

func TestGenerationServiceTierPreservesPreparedChoice(t *testing.T) {
	for _, tc := range serviceTierProtocolCases() {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Replace(handoffUnitStart, `"serviceTier":null`, `"serviceTier":`+tc.prepared, 1)
			f := handoffUnitFromStart(t, start)
			f.p.generation = newGenerationProtocol(f.p.thread)
			f.ready(t)
			params := map[string]any{
				"threadId": f.p.thread.ID,
				"input":    []any{map[string]any{"type": "text", "text": "question"}},
				// Unlike serviceTier, serviceTierForTurn:null inherits in 0.157.1.
				"serviceTierForTurn": nil,
			}
			if tc.request != "" {
				params["serviceTier"] = json.RawMessage(tc.request)
			}
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "go", "turn/start", params), true, false, !tc.accept)
			g := f.p.generation
			if !tc.accept {
				if g.active || g.firstKey != "" || g.startKey != "" || len(f.p.pending) != 0 {
					t.Fatal("rejected tier consumed the initial turn gate")
				}
				return
			}
			if !g.active || g.firstPrompt != "question" || !sameJSON(f.p.settings["serviceTier"], json.RawMessage(tc.prepared)) {
				t.Fatal("accepted turn lost its prompt or acknowledged tier")
			}
		})
	}
}

func TestGenerationCorrelatesApprovalsAndInterruption(t *testing.T) {
	f := generationUnit(t)
	generationStart(t, f, "go")
	generationNotice(t, f, "turn/started", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("inProgress")}, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "go", map[string]any{"turn": generationTurn("inProgress")}), false, false, false)
	request := func(turn string) []byte {
		return handoffUnitRequest(t, "approval", "item/commandExecution/requestApproval", map[string]any{"threadId": f.p.thread.ID, "turnId": turn, "itemId": "item-1"})
	}
	handoffUnitObserve(t, f.p, request("wrong"), false, false, true)
	handoffUnitObserve(t, f.p, request("turn-1"), false, false, false)
	handoffUnitObserve(t, f.p, request("turn-1"), false, false, true)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "unknown", map[string]any{"decision": "accept"}), true, false, true)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "approval", map[string]any{"decision": "decline"}), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "approval", map[string]any{"decision": "accept"}), true, false, true)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "cancel", "turn/interrupt", map[string]any{"threadId": f.p.thread.ID, "turnId": "wrong"}), true, false, true)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "cancel", "turn/interrupt", map[string]any{"threadId": f.p.thread.ID, "turnId": "turn-1"}), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "cancel", map[string]any{}), false, false, false)
	generationNotice(t, f, "turn/completed", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("interrupted")}, false)
	if f.p.generation.active || f.p.generation.completed == nil || f.p.generation.completed.ExecutionKnown {
		t.Fatal("interrupted request is not proven unexecuted")
	}
}

func TestGenerationNoOverlappingTurnsAndNoRepeatedPreparation(t *testing.T) {
	f := generationUnit(t)
	generationStart(t, f, "go")
	start := handoffUnitRequest(t, "next", "turn/start", map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "second question"}}})
	handoffUnitObserve(t, f.p, start, true, false, true)
	generationNotice(t, f, "turn/started", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("inProgress")}, false)
	generationNotice(t, f, "turn/completed", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("completed")}, false)
	handoffUnitObserve(t, f.p, start, true, false, true)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "go", map[string]any{"turn": generationTurn("inProgress")}), false, false, false)
	handoffUnitObserve(t, f.p, start, true, false, false)
	if f.p.generation.firstPrompt != "first question" || f.p.generation.firstKey != "s:go" {
		t.Fatal("second prompt replaced first binding")
	}
}

func TestGenerationRejectsForeignAcknowledgementsAndNotifications(t *testing.T) {
	f := generationUnit(t)
	generationStart(t, f, "go")
	generationNotice(t, f, "turn/started", map[string]any{"threadId": "foreign", "turn": generationTurn("inProgress")}, true)
	generationNotice(t, f, "turn/started", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("inProgress")}, false)
	generationNotice(t, f, "thread/tokenUsage/updated", map[string]any{"threadId": f.p.thread.ID, "turnId": "other"}, true)
	turn := generationTurn("inProgress")
	turn["id"] = "other"
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "go", map[string]any{"turn": turn}), false, false, true)
}

func TestGenerationRejectNeverProvesNonExecution(t *testing.T) {
	f := generationUnit(t)
	generationStart(t, f, "go")
	handoffUnitObserve(t, f.p, []byte(`{"id":"go","error":{"code":-32000,"message":"PRIVATE_BODY"}}`), false, false, false)
	o := f.p.generation.completed
	if o == nil || o.ExecutionKnown || o.Outcome != "unknown" {
		t.Fatal("RPC error fabricated non-execution")
	}
	raw, err := json.Marshal(PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "PRIVATE_BODY"}}})
	if err != nil || string(raw) != "{}" {
		t.Fatalf("private preparation serialized: %s %v", raw, err)
	}
}

func TestGenerationDeniesAuxiliaryTitleWithoutGrantingAnotherThread(t *testing.T) {
	f := generationUnit(t)
	params := map[string]any{"ephemeral": true, "threadSource": "thread_title"}
	request := handoffUnitRequest(t, "title", "thread/start", params)
	for i := 0; i < 16; i++ {
		_, err := f.p.observe(request, true)
		denied, ok := err.(*deniedAuxiliaryRequest)
		if !ok || string(denied.id) != `"title"` || len(f.p.pending) != 0 {
			t.Fatalf("invalid local denial: %T", err)
		}
	}
	if _, err := f.p.observe(request, true); err == nil {
		t.Fatal("unbounded auxiliary traffic")
	}
	// The original conversation remains usable; denial never consumes its gate.
	generationStart(t, f, "original")
	for _, p := range []map[string]any{{"ephemeral": false, "threadSource": "thread_title"}, {"ephemeral": true, "threadSource": "user"}} {
		_, err := f.p.observe(handoffUnitRequest(t, "new", "thread/start", p), true)
		if err == nil {
			t.Fatal("additional thread allowed")
		}
		if _, denied := err.(*deniedAuxiliaryRequest); denied {
			t.Fatal("non-title request treated as auxiliary")
		}
	}
}

func TestGenerationPreservesAcknowledgedRootsAndDisabledPlugins(t *testing.T) {
	for _, tc := range []struct{ key, baseline, accepted, rejected string }{
		{"runtimeWorkspaceRoots", `["/work","/second"]`, `["/work","/second"]`, `["/work"]`},
		{"disabledPluginIds", `["disabled-plugin"]`, `["disabled-plugin"]`, `[]`},
	} {
		for _, raw := range []string{tc.accepted, tc.rejected, "null"} {
			f := generationUnit(t)
			f.p.settings[tc.key] = json.RawMessage(tc.baseline)
			params := map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "question"}}, tc.key: json.RawMessage(raw)}
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "go", "turn/start", params), true, false, raw == tc.rejected)
		}
		f := generationUnit(t)
		params := map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "question"}}, tc.key: json.RawMessage(tc.accepted)}
		handoffUnitObserve(t, f.p, handoffUnitRequest(t, "unknown-baseline", "turn/start", params), true, false, true)
	}
}

func TestGenerationRejectsNotificationSuppressionBeforeInitialize(t *testing.T) {
	for _, method := range []string{"model/rerouted", "thread/compacted", "item/started", "thread/tokenUsage/updated", "error", "future-event"} {
		f := newHandoffUnitFixture(t)
		f.p.generation = newGenerationProtocol(f.p.thread)
		params := map[string]any{"capabilities": map[string]any{"experimentalApi": true, "optOutNotificationMethods": []string{method}}}
		handoffUnitObserve(t, f.p, handoffUnitRequest(t, "init", "initialize", params), true, false, true)
		if f.p.initSent || len(f.p.pending) != 0 {
			t.Fatal("suppressed stream was admitted")
		}
	}
	f := newHandoffUnitFixture(t)
	f.p.generation = newGenerationProtocol(f.p.thread)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "init", "initialize", map[string]any{"capabilities": map[string]any{"experimentalApi": true, "optOutNotificationMethods": []string{}}}), true, false, false)
}
