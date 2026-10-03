package nativecodex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// These are protocol payloads only. WebSocket frame kinds, relay delivery, and
// native TUI behavior belong to the transport tests.
const handoffUnitStart = `{"model":"fixture-model","modelProvider":"fixture-provider","cwd":"/work","approvalPolicy":"untrusted","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"serviceTier":null,"reasoningEffort":null,"thread":{"id":"prepared-thread","cwd":"/work","turns":[]}}`

const handoffUnitSentinel = "PRIVATE_HANDOFF_RAW_SENTINEL"

type handoffUnitFixture struct {
	p      *handoffProtocol
	result json.RawMessage
}

func newHandoffUnitFixture(t *testing.T) handoffUnitFixture {
	t.Helper()
	return handoffUnitFromStart(t, handoffUnitStart)
}

func handoffUnitFromStart(t *testing.T, start string) handoffUnitFixture {
	t.Helper()
	thread, ok := freshThread([]byte(start), "/work", "fixture-provider")
	if !ok {
		t.Fatal("fixture must satisfy the fresh thread contract")
	}
	thread.SettingsHash, ok = threadSettings([]byte(start), thread, ThreadOptions{
		Model: "fixture-model", ModelProvider: "fixture-provider", Sandbox: "read-only", ApprovalPolicy: "untrusted",
	})
	if !ok || thread.SettingsHash == "" {
		t.Fatal("fixture must bind the acknowledged settings")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(start), &result); err != nil {
		t.Fatal(err)
	}
	// thread/start does not contain these resume-only fields. In particular,
	// null reasoning effort must remain null in the collaboration settings.
	result["collaborationMode"] = handoffUnitJSON(t, map[string]any{
		"mode": "default", "settings": map[string]any{
			"model": thread.Model, "reasoning_effort": result["reasoningEffort"], "developer_instructions": nil,
		},
	})
	for _, key := range []string{"initialTurnsPage", "turnsBackwardsCursor", "itemsBackwardsCursor"} {
		result[key] = json.RawMessage(`null`)
	}
	return handoffUnitFixture{newHandoffProtocol(thread, json.RawMessage(start), "cached"), handoffUnitJSON(t, result)}
}

func handoffUnitJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func handoffUnitRequest(t *testing.T, id any, method string, params any) []byte {
	t.Helper()
	return handoffUnitJSON(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func handoffUnitResponse(t *testing.T, id any, result any) []byte {
	t.Helper()
	return handoffUnitJSON(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func handoffUnitObserve(t *testing.T, p *handoffProtocol, raw []byte, client, wantReady, wantReject bool) {
	t.Helper()
	ready, err := p.observe(raw, client)
	if wantReject {
		if ready || !errors.Is(err, ErrProtocol) {
			t.Fatalf("rejected frame: ready=%v, err=%v; want false, ErrProtocol", ready, err)
		}
		if strings.Contains(err.Error(), handoffUnitSentinel) {
			t.Fatal("protocol error leaked raw peer content")
		}
		return
	}
	if err != nil || ready != wantReady {
		t.Fatalf("observe: ready=%v, err=%v; want ready=%v", ready, err, wantReady)
	}
}

func (f handoffUnitFixture) initialize(t *testing.T) {
	t.Helper()
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "init", "initialize", map[string]any{"clientInfo": map[string]any{"name": "fixture", "version": "1"}}), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "init", map[string]any{"userAgent": "fixture/1"}), false, false, false)
	handoffUnitObserve(t, f.p, []byte(`{"method":"initialized"}`), true, false, false)
}

func (f handoffUnitFixture) resume(t *testing.T, id any) {
	t.Helper()
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, id, "thread/resume", map[string]any{"threadId": "prepared-thread", "excludeTurns": true}), true, false, false)
}

func (f handoffUnitFixture) ready(t *testing.T) {
	t.Helper()
	f.initialize(t)
	f.resume(t, "resume")
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
}

func handoffUnitReplace(t *testing.T, raw []byte, from, to string) []byte {
	t.Helper()
	if !strings.Contains(string(raw), from) {
		t.Fatalf("fixture replacement missing %q", from)
	}
	return []byte(strings.Replace(string(raw), from, to, 1))
}

func TestHandoffProtocolReadinessRequiresCorrelatedResume(t *testing.T) {
	f := newHandoffUnitFixture(t)
	f.initialize(t)
	for _, method := range []string{"thread/loaded/list", "thread/list", "thread/read"} {
		handoffUnitObserve(t, f.p, handoffUnitRequest(t, method, method, map[string]any{"threadId": "prepared-thread"}), true, false, false)
	}
	f.resume(t, "resume")
	// Even an exact copy of the prepared identity/settings in an unrelated
	// inspection result is not evidence of a successful resume.
	for _, method := range []string{"thread/read", "thread/list", "thread/loaded/list"} {
		handoffUnitObserve(t, f.p, handoffUnitResponse(t, method, f.result), false, false, false)
	}
	notification := handoffUnitJSON(t, map[string]any{"method": "thread/resumed", "params": f.result})
	handoffUnitObserve(t, f.p, notification, false, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
	handoffUnitObserve(t, f.p, notification, false, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, false, true)
}

func TestHandoffProtocolInitializationOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  int // 0: new, 1: initialize sent, 2: acknowledged, 3: initialized
		raw    string
		client bool
	}{
		{"notification before initialize", 0, `{"method":"initialized"}`, true},
		{"notification before ack", 1, `{"method":"initialized"}`, true},
		{"resume before initialize", 0, `{"id":2,"method":"thread/resume","params":{"threadId":"prepared-thread"}}`, true},
		{"resume before initialized", 2, `{"id":2,"method":"thread/resume","params":{"threadId":"prepared-thread"}}`, true},
		{"read before initialized", 2, `{"id":2,"method":"config/read","params":{}}`, true},
		{"unsolicited ack", 0, `{"id":"init","result":{"userAgent":"fixture"}}`, false},
		{"wrong ack ID", 1, `{"id":"other","result":{"userAgent":"fixture"}}`, false},
		{"empty ack", 1, `{"id":"init","result":{}}`, false},
		{"null ack", 1, `{"id":"init","result":null}`, false},
		{"null host", 1, `{"id":"init","result":{"userAgent":null}}`, false},
		{"numeric host", 1, `{"id":"init","result":{"userAgent":1}}`, false},
		{"empty host", 1, `{"id":"init","result":{"userAgent":""}}`, false},
		{"aliased ack", 1, `{"id":"init","result":{"UserAgent":"other","userAgent":"fixture"}}`, false},
		{"duplicate initialize", 1, `{"id":2,"method":"initialize","params":{}}`, true},
		{"reinitialize after ack", 3, `{"id":2,"method":"initialize","params":{}}`, true},
		{"repeated initialized", 3, `{"method":"initialized"}`, true},
		{"initialized is not a request", 2, `{"id":2,"method":"initialized","params":{}}`, true},
		{"client cannot acknowledge", 1, `{"id":"init","result":{"userAgent":"fixture"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if tc.stage >= 1 {
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, "init", "initialize", map[string]any{}), true, false, false)
			}
			if tc.stage >= 2 {
				handoffUnitObserve(t, f.p, handoffUnitResponse(t, "init", map[string]any{"userAgent": "fixture"}), false, false, false)
			}
			if tc.stage >= 3 {
				handoffUnitObserve(t, f.p, []byte(`{"method":"initialized"}`), true, false, false)
			}
			handoffUnitObserve(t, f.p, []byte(tc.raw), tc.client, false, true)
		})
	}
}

func TestHandoffProtocolReadOnlyOperations(t *testing.T) {
	methods := []string{
		"thread/read", "thread/turns/list", "thread/items/list", "thread/goal/get",
		"account/read", "config/read", "configRequirements/read", "hooks/list", "model/list",
		"collaborationMode/list", "thread/loaded/list", "thread/list", "skills/list", "plugin/list", "app/list", "mcpServerStatus/list",
	}
	for _, afterACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterACK=%v", afterACK), func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if afterACK {
				f.ready(t)
			} else {
				f.initialize(t)
			}
			for _, method := range methods {
				t.Run(method, func(t *testing.T) {
					handoffUnitObserve(t, f.p, handoffUnitRequest(t, method, method, map[string]any{"threadId": "prepared-thread"}), true, false, false)
					handoffUnitObserve(t, f.p, handoffUnitResponse(t, method, map[string]any{}), false, false, false)
					handoffUnitObserve(t, f.p, handoffUnitRequest(t, method, method, map[string]any{"threadId": "prepared-thread"}), true, false, false)
					handoffUnitObserve(t, f.p, handoffUnitJSON(t, map[string]any{"id": method, "error": map[string]any{"code": -32000, "message": handoffUnitSentinel}}), false, false, false)
				})
			}
		})
	}
	for _, method := range methods[:4] {
		for _, threadID := range []any{"other-thread", nil, 1} {
			t.Run(fmt.Sprintf("wrong thread/%s/%v", method, threadID), func(t *testing.T) {
				f := newHandoffUnitFixture(t)
				f.initialize(t)
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, 1, method, map[string]any{"threadId": threadID}), true, false, true)
			})
		}
	}
}

func TestHandoffProtocolNullInspectionParameters(t *testing.T) {
	for _, afterACK := range []bool{false, true} {
		for _, params := range []string{``, `,"params":null`, `,"params":{}`} {
			for _, method := range []string{"configRequirements/read", "config/read", "thread/loaded/list"} {
				t.Run(fmt.Sprintf("afterACK=%v/%s/%s", afterACK, method, params), func(t *testing.T) {
					f := newHandoffUnitFixture(t)
					if afterACK {
						f.ready(t)
					} else {
						f.initialize(t)
					}
					raw := []byte(`{"id":"inspect","method":"` + method + `"` + params + `}`)
					handoffUnitObserve(t, f.p, raw, true, false, false)
					handoffUnitObserve(t, f.p, handoffUnitResponse(t, "inspect", map[string]any{}), false, false, false)
				})
			}
			for _, method := range []string{"thread/read", "thread/turns/list", "thread/items/list", "thread/goal/get", "thread/resume", "turn/start", "config/value/write"} {
				t.Run(fmt.Sprintf("no bypass/afterACK=%v/%s/%s", afterACK, method, params), func(t *testing.T) {
					f := newHandoffUnitFixture(t)
					if afterACK {
						f.ready(t)
					} else {
						f.initialize(t)
					}
					handoffUnitObserve(t, f.p, []byte(`{"id":"inspect","method":"`+method+`"`+params+`}`), true, false, true)
				})
			}
		}
	}
}

func TestHandoffProtocolNotificationTimestamp(t *testing.T) {
	for _, timestamp := range []string{`0`, `1790985600000`, `18446744073709551615`} {
		for _, stage := range []string{"new", "initialized", "resumed"} {
			t.Run(stage+"/"+timestamp, func(t *testing.T) {
				f := newHandoffUnitFixture(t)
				if stage == "initialized" {
					f.initialize(t)
				} else if stage == "resumed" {
					f.ready(t)
				}
				handoffUnitObserve(t, f.p, []byte(`{"method":"thread/resumed","params":{"threadId":"prepared-thread"},"emittedAtMs":`+timestamp+`}`), false, false, false)
			})
		}
	}
	for _, timestamp := range []string{`null`, `true`, `"1790985600000"`, `[]`, `{}`, `-1`, `-0`, `1.5`, `1.0`, `1e3`, `18446744073709551616`} {
		t.Run("invalid type/"+timestamp, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			handoffUnitObserve(t, f.p, []byte(`{"method":"thread/resumed","emittedAtMs":`+timestamp+`}`), false, false, true)
		})
	}
	for _, field := range []string{"EmittedAtMs", "emittedatms", "emittedAtMS"} {
		for _, canonical := range []string{``, `,"emittedAtMs":1`} {
			t.Run("alias/"+field+canonical, func(t *testing.T) {
				f := newHandoffUnitFixture(t)
				handoffUnitObserve(t, f.p, []byte(`{"method":"thread/resumed","`+field+`":1`+canonical+`}`), false, false, true)
			})
		}
	}
	for _, field := range []string{`emittedAtMs`, `emittedAtM\u0073`} {
		t.Run("duplicate/"+field, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			handoffUnitObserve(t, f.p, []byte(`{"method":"thread/resumed","emittedAtMs":1,"`+field+`":2}`), false, false, true)
		})
	}
	t.Run("client notification", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		handoffUnitObserve(t, f.p, handoffUnitRequest(t, "init", "initialize", map[string]any{}), true, false, false)
		handoffUnitObserve(t, f.p, handoffUnitResponse(t, "init", map[string]any{"userAgent": "fixture"}), false, false, false)
		handoffUnitObserve(t, f.p, []byte(`{"method":"initialized","emittedAtMs":1}`), true, false, true)
	})
	t.Run("client request", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		f.initialize(t)
		handoffUnitObserve(t, f.p, []byte(`{"id":1,"method":"configRequirements/read","params":null,"emittedAtMs":1}`), true, false, true)
	})
	t.Run("server response", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		f.initialize(t)
		f.resume(t, "resume")
		ack := handoffUnitResponse(t, "resume", f.result)
		handoffUnitObserve(t, f.p, append([]byte(`{"emittedAtMs":1,`), ack[1:]...), false, false, true)
	})
	t.Run("server request", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		handoffUnitObserve(t, f.p, []byte(`{"id":1,"method":"thread/resumed","emittedAtMs":1}`), false, false, true)
	})
	t.Run("server notification with null ID", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		handoffUnitObserve(t, f.p, []byte(`{"id":null,"method":"thread/resumed","emittedAtMs":1}`), false, false, true)
	})
}

func TestHandoffProtocolResumeParametersPreserveIntent(t *testing.T) {
	for _, params := range []string{
		`{"threadId":"prepared-thread"}`,
		`{"threadId":"prepared-thread","model":null,"modelProvider":null,"cwd":null,"approvalPolicy":null,"approvalsReviewer":null,"serviceTier":null,"sandbox":null,"config":null,"history":null,"path":null,"baseInstructions":null,"developerInstructions":null,"personality":null,"permissions":null,"runtimeWorkspaceRoots":null,"initialTurnsPage":null,"excludeTurns":true}`,
		`{"threadId":"prepared-thread","model":"fixture-model","modelProvider":"fixture-provider","cwd":"/work","approvalPolicy":"untrusted","approvalsReviewer":"user","sandbox":"read-only","config":{"web_search":"cached"},"excludeTurns":true}`,
		`{"threadId":"prepared-thread","config":{}}`,
	} {
		t.Run(params, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "resume", "thread/resume", json.RawMessage(params)), true, false, false)
			handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
		})
	}
	for _, tc := range []struct{ name, field, value string }{
		{"wrong thread", "threadId", `"other-thread"`},
		{"null thread", "threadId", `null`},
		{"changed model", "model", `"other-model"`},
		{"changed provider", "modelProvider", `"other-provider"`},
		{"changed cwd", "cwd", `"/other"`},
		{"changed approval", "approvalPolicy", `"never"`},
		{"changed reviewer", "approvalsReviewer", `"auto_review"`},
		{"changed tier", "serviceTier", `"fast"`},
		{"changed sandbox", "sandbox", `"danger-full-access"`},
		{"unknown sandbox", "sandbox", `"unknown"`},
		{"sandbox object", "sandbox", `{"type":"readOnly"}`},
		{"changed search", "config", `{"web_search":"live"}`},
		{"null search", "config", `{"web_search":null}`},
		{"extra config", "config", `{"web_search":"cached","model":"fixture-model"}`},
		{"config instructions", "config", `{"developer_instructions":"` + handoffUnitSentinel + `"}`},
		{"config permissions", "config", `{"sandbox_mode":"danger-full-access"}`},
		{"aliased config", "config", `{"Web_Search":"cached"}`},
		{"duplicate config", "config", `{"web_search":"live","web_search":"cached"}`},
		{"config array", "config", `[]`},
		{"history", "history", `[]`},
		{"path", "path", `"/other/rollout.jsonl"`},
		{"base instructions", "baseInstructions", `"` + handoffUnitSentinel + `"`},
		{"developer instructions", "developerInstructions", `"` + handoffUnitSentinel + `"`},
		{"personality", "personality", `"friendly"`},
		{"permissions", "permissions", `{}`},
		{"workspace roots", "runtimeWorkspaceRoots", `[]`},
		{"initial turns", "initialTurnsPage", `{}`},
		{"include turns", "excludeTurns", `false`},
		{"null exclude turns", "excludeTurns", `null`},
		{"string exclude turns", "excludeTurns", `"true"`},
		{"unknown optional field", "futureSetting", `null`},
		{"aliased thread", "ThreadId", `"prepared-thread"`},
		{"aliased model", "Model", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			params := map[string]json.RawMessage{"threadId": json.RawMessage(`"prepared-thread"`)}
			params[tc.field] = json.RawMessage(tc.value)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "resume", "thread/resume", params), true, false, true)
		})
	}
}

func TestHandoffProtocolResumeResultMatchesPreparedSettings(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"wrong thread", `"id":"prepared-thread"`, `"id":"other-thread"`},
		{"wrong model", `"model":"fixture-model","modelProvider"`, `"model":"other-model","modelProvider"`},
		{"wrong provider", `"modelProvider":"fixture-provider"`, `"modelProvider":"other-provider"`},
		{"wrong cwd", `"cwd":"/work"`, `"cwd":"/other"`},
		{"wrong thread cwd", `"id":"prepared-thread","cwd":"/work"`, `"id":"prepared-thread","cwd":"/other"`},
		{"wrong approval", `"approvalPolicy":"untrusted"`, `"approvalPolicy":"never"`},
		{"wrong reviewer", `"approvalsReviewer":"user"`, `"approvalsReviewer":"auto_review"`},
		{"wrong sandbox", `"type":"readOnly"`, `"type":"workspaceWrite"`},
		{"changed network permission", `"networkAccess":false`, `"networkAccess":true`},
		{"changed effort", `"reasoningEffort":null`, `"reasoningEffort":"high"`},
		{"missing effort", `"reasoningEffort":null,`, ``},
		{"added instructions", `"serviceTier":null`, `"serviceTier":null,"developerInstructions":"` + handoffUnitSentinel + `"`},
		{"added config", `"serviceTier":null`, `"serviceTier":null,"config":{"web_search":"cached"}`},
		{"added permission", `"serviceTier":null`, `"serviceTier":null,"permissions":{}`},
		{"future native setting", `"serviceTier":null`, `"serviceTier":null,"futureSetting":null`},
		{"nonfresh turns", `"turns":[]`, `"turns":[{}]`},
		{"null turns", `"turns":[]`, `"turns":null`},
		{"missing turns", `,"turns":[]`, ``},
		{"wrong thread shape", `"thread":{`, `"thread":null,"unused":{`},
		{"duplicate identity", `"id":"prepared-thread"`, `"id":"other-thread","id":"prepared-thread"`},
		{"aliased identity", `"id":"prepared-thread"`, `"Id":"other-thread","id":"prepared-thread"`},
		{"duplicate settings", `"approvalPolicy":"untrusted"`, `"approvalPolicy":"never","approvalPolicy":"untrusted"`},
		{"aliased settings", `"approvalPolicy":"untrusted"`, `"ApprovalPolicy":"never","approvalPolicy":"untrusted"`},
		{"aliased sandbox", `"type":"readOnly"`, `"Type":"dangerFullAccess","type":"readOnly"`},
		{"collaboration mode", `"mode":"default"`, `"mode":"plan"`},
		{"collaboration model", `"developer_instructions":null,"model":"fixture-model"`, `"developer_instructions":null,"model":"other-model"`},
		{"collaboration effort", `"reasoning_effort":null`, `"reasoning_effort":"high"`},
		{"collaboration instructions", `"developer_instructions":null`, `"developer_instructions":"` + handoffUnitSentinel + `"`},
		{"extra collaboration instructions", `"mode":"default"`, `"mode":"default","developer_instructions":"` + handoffUnitSentinel + `"`},
		{"extra collaboration config", `"reasoning_effort":null`, `"reasoning_effort":null,"config":{"web_search":"live"}`},
		{"extra collaboration permission", `"reasoning_effort":null`, `"reasoning_effort":null,"permissions":{"network":true}`},
		{"missing collaboration instructions", `"developer_instructions":null,`, ``},
		{"aliased collaboration instructions", `"developer_instructions":null`, `"Developer_Instructions":"` + handoffUnitSentinel + `","developer_instructions":null`},
		{"duplicate collaboration model", `"reasoning_effort":null`, `"reasoning_effort":null,"model":"other-model"`},
		{"aliased collaboration field", `"collaborationMode":`, `"CollaborationMode":null,"collaborationMode":`},
		{"initial turns page", `"initialTurnsPage":null`, `"initialTurnsPage":{}`},
		{"turns cursor", `"turnsBackwardsCursor":null`, `"turnsBackwardsCursor":"next"`},
		{"items cursor", `"itemsBackwardsCursor":null`, `"itemsBackwardsCursor":"next"`},
		{"aliased pagination", `"itemsBackwardsCursor":null`, `"ItemsBackwardsCursor":null,"itemsBackwardsCursor":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			f.resume(t, "resume")
			result := handoffUnitReplace(t, f.result, tc.from, tc.to)
			handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", json.RawMessage(result)), false, false, true)
		})
	}
	for _, raw := range []string{`null`, `[]`, `"` + handoffUnitSentinel + `"`, `{}`, `{"thread":null}`} {
		t.Run("malformed/"+raw, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			f.resume(t, "resume")
			handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", json.RawMessage(raw)), false, false, true)
		})
	}
}

func TestHandoffProtocolResumeResultAllowsEquivalentSettings(t *testing.T) {
	for _, start := range []string{
		handoffUnitStart,
		strings.Replace(handoffUnitStart, `"reasoningEffort":null`, `"reasoningEffort":"high"`, 1),
		strings.Replace(handoffUnitStart, `"serviceTier":null`, `"serviceTier":null,"futureSetting":{"b":2,"a":9007199254740993}`, 1),
	} {
		for _, collaboration := range []string{"matching", "null", "absent"} {
			t.Run(start+"/"+collaboration, func(t *testing.T) {
				f := handoffUnitFromStart(t, start)
				f.initialize(t)
				f.resume(t, "resume")
				var result map[string]json.RawMessage
				if err := json.Unmarshal(f.result, &result); err != nil {
					t.Fatal(err)
				}
				if collaboration == "null" {
					result["collaborationMode"] = json.RawMessage(`null`)
				} else if collaboration == "absent" {
					delete(result, "collaborationMode")
					delete(result, "initialTurnsPage")
					delete(result, "turnsBackwardsCursor")
					delete(result, "itemsBackwardsCursor")
				}
				// Metadata is not part of runtime settings, and object key order
				// (including nested settings) must not change the settings hash.
				result["thread"] = json.RawMessage(`{"turns":[],"cwd":"/work","id":"prepared-thread","updatedAt":42}`)
				result["sandbox"] = json.RawMessage(`{"networkAccess":false,"type":"readOnly"}`)
				if result["futureSetting"] != nil {
					result["futureSetting"] = json.RawMessage(`{"a":9007199254740993,"b":2}`)
				}
				handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", result), false, true, false)
			})
		}
	}
}

func TestHandoffProtocolCorrelatesTypedIDsOutOfOrder(t *testing.T) {
	f := newHandoffUnitFixture(t)
	f.initialize(t)
	// A numeric inspection ID and a string resume ID with the same digits
	// must coexist, and each response must discharge its own request.
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, 7, "thread/loaded/list", map[string]any{}), true, false, false)
	f.resume(t, "7")
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "later", "config/read", map[string]any{}), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "later", map[string]any{}), false, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, 7, f.result), false, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "7", f.result), false, true, false)
	for _, id := range []any{7, "7", "unknown"} {
		handoffUnitObserve(t, f.p, handoffUnitResponse(t, id, f.result), false, false, true)
	}
	t.Run("resume completes before earlier read", func(t *testing.T) {
		f := newHandoffUnitFixture(t)
		f.initialize(t)
		handoffUnitObserve(t, f.p, handoffUnitRequest(t, "7", "config/read", map[string]any{}), true, false, false)
		f.resume(t, 7)
		handoffUnitObserve(t, f.p, handoffUnitResponse(t, 7, f.result), false, true, false)
		handoffUnitObserve(t, f.p, handoffUnitResponse(t, "7", map[string]any{}), false, false, false)
	})
}

func TestHandoffProtocolPendingBound(t *testing.T) {
	for _, afterACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterACK=%v", afterACK), func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if afterACK {
				f.ready(t)
			} else {
				f.initialize(t)
			}
			for id := 0; id < 64; id++ {
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, id, "config/read", map[string]any{}), true, false, false)
			}
			// A response, including an inspection error, releases its slot.
			handoffUnitObserve(t, f.p, []byte(`{"id":31,"error":{"code":-32000,"message":"unavailable"}}`), false, false, false)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, 64, "config/read", map[string]any{}), true, false, false)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, 65, "config/read", map[string]any{}), true, false, true)
		})
	}
}

func TestHandoffProtocolRejectsAmbiguousEnvelopesAndIDs(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{} {}`, `{"id":`,
		`{"id":1,"id":2,"method":"config/read","params":{}}`,
		`{"id":1,"\u0069d":2,"method":"config/read","params":{}}`,
		`{"id":1,"ID":2,"method":"config/read","params":{}}`,
		`{"id":1,"Method":"turn/start","method":"config/read","params":{}}`,
		`{"id":1,"method":"config/read","method":"turn/start","params":{}}`,
		`{"id":1,"method":"config/read","Params":{},"params":{}}`,
		`{"id":1,"method":"config/read","params":{},"extra":null}`,
		`{"id":1,"method":"config/read","params":{},"result":{}}`,
		`{"id":1,"method":"config/read","params":{},"error":null}`,
		`{"id":1,"method":null,"params":{}}`,
		`{"id":1,"method":"","params":{}}`,
		`{"id":1,"method":1,"params":{}}`,
		`{"id":1,"method":"config/read","params":[]}`,
		`{"id":1,"method":"config/read","params":{"cwd":"/work","cwd":"/other"}}`,
		`{"jsonrpc":"1.0","id":1,"method":"config/read","params":{}}`,
		`{"jsonrpc":2.0,"id":1,"method":"config/read","params":{}}`,
		`{"JSONRPC":"2.0","id":1,"method":"config/read","params":{}}`,
		"{\"method\":\"config/read\",\"params\":\"\xff\"}",
	} {
		t.Run(raw, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, []byte(raw), true, false, true)
		})
	}
	for _, id := range []string{`null`, `true`, `{}`, `[]`, `-1`, `1.0`, `1e0`, `18446744073709551616`, `""`, `"a\n"`, `"a\u007f"`, `"` + strings.Repeat("a", 129) + `"`} {
		t.Run("invalid ID/"+id, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, []byte(`{"id":`+id+`,"method":"config/read","params":{}}`), true, false, true)
		})
	}
	for _, id := range []string{`0`, `18446744073709551615`, `"` + strings.Repeat("a", 128) + `"`} {
		t.Run("valid ID boundary/"+id, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, []byte(`{"id":`+id+`,"method":"config/read","params":{}}`), true, false, false)
			handoffUnitObserve(t, f.p, []byte(`{"id":`+id+`,"result":{}}`), false, false, false)
		})
	}
	for _, id := range []string{`"busy"`, `"bu\u0073y"`} {
		t.Run("duplicate pending ID/"+id, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "busy", "config/read", map[string]any{}), true, false, false)
			handoffUnitObserve(t, f.p, []byte(`{"id":`+id+`,"method":"model/list","params":{}}`), true, false, true)
		})
	}
}

func TestHandoffProtocolRejectsMalformedResponses(t *testing.T) {
	for _, raw := range []string{
		`{"id":"resume"}`,
		`{"result":{}}`,
		`{"id":"unknown","result":{}}`,
		`{"id":null,"result":{}}`,
		`{"id":"resume","result":{},"error":null}`,
		`{"id":"resume","result":{},"params":null}`,
		`{"id":"resume","result":{},"Result":{}}`,
		`{"id":"resume","result":{},"result":{}}`,
		`{"id":"resume","ID":"other","result":{}}`,
		`{"id":"resume","id":"other","result":{}}`,
		`{"id":"resume","error":{},"Error":{}}`,
		`{"id":"resume","error":{},"error":{}}`,
		`{"id":"resume","method":"thread/resumed","params":{}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			f.resume(t, "resume")
			handoffUnitObserve(t, f.p, []byte(raw), false, false, true)
		})
	}
}

func TestHandoffProtocolResumeIsOneShot(t *testing.T) {
	for _, outcome := range []string{"pending", "acknowledged", "rejected", "malformed"} {
		t.Run(outcome, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			f.resume(t, "resume")
			switch outcome {
			case "acknowledged":
				handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
			case "rejected":
				handoffUnitObserve(t, f.p, []byte(`{"id":"resume","error":{"message":"`+handoffUnitSentinel+`"}}`), false, false, true)
			case "malformed":
				handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", nil), false, false, true)
			}
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "retry", "thread/resume", map[string]any{"threadId": "prepared-thread"}), true, false, true)
		})
	}
}

func TestHandoffProtocolDeniesGenerationAndMutationEvenAfterACK(t *testing.T) {
	methods := []string{
		"turn/start", "turn/steer", "turn/interrupt", "thread/start", "thread/fork", "thread/inject_items",
		"thread/archive", "thread/rollback", "thread/compact/start", "thread/name/set", "thread/goal/set",
		"config/value/write", "config/batchWrite", "command/exec", "fs/writeFile", "account/login/start",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/call",
		"permissions/requestApproval", "future/mutation",
	}
	for _, afterACK := range []bool{false, true} {
		for _, method := range methods {
			for _, client := range []bool{false, true} {
				t.Run(fmt.Sprintf("afterACK=%v/client=%v/%s", afterACK, client, method), func(t *testing.T) {
					f := newHandoffUnitFixture(t)
					if afterACK {
						f.ready(t)
					} else {
						f.initialize(t)
					}
					handoffUnitObserve(t, f.p, handoffUnitRequest(t, "mutate", method, map[string]any{"threadId": "prepared-thread", "input": handoffUnitSentinel}), client, false, true)
				})
			}
		}
		t.Run(fmt.Sprintf("client notification/afterACK=%v", afterACK), func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if afterACK {
				f.ready(t)
			} else {
				f.initialize(t)
			}
			handoffUnitObserve(t, f.p, []byte(`{"method":"turn/start","params":{"threadId":"prepared-thread"}}`), true, false, true)
		})
	}
}

func TestHandoffProtocolErrorsDoNotLeakRawContent(t *testing.T) {
	for _, method := range []string{"initialize", "thread/resume"} {
		t.Run(method, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if method == "initialize" {
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, "sensitive", method, map[string]any{}), true, false, false)
			} else {
				f.initialize(t)
				f.resume(t, "sensitive")
			}
			handoffUnitObserve(t, f.p, handoffUnitJSON(t, map[string]any{"id": "sensitive", "error": map[string]any{
				"code": -32000, "message": handoffUnitSentinel, "data": map[string]any{"raw": handoffUnitSentinel},
			}}), false, false, true)
		})
	}
	for _, raw := range []string{
		`{"id":"` + handoffUnitSentinel + `","result":{}}`,
		`{"id":1,"method":"` + handoffUnitSentinel + `","params":{}}`,
		`{"` + handoffUnitSentinel + `":null}`,
		`{"method":"` + handoffUnitSentinel,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.initialize(t)
			handoffUnitObserve(t, f.p, []byte(raw), strings.Contains(raw, `"method"`), false, true)
		})
	}
}

func TestHandoffProtocolConcurrentResumeAcknowledgement(t *testing.T) {
	f := newHandoffUnitFixture(t)
	f.initialize(t)
	f.resume(t, "resume")
	ack := handoffUnitResponse(t, "resume", f.result)
	type observation struct {
		ready bool
		err   error
	}
	const workers = 16
	results := make(chan observation, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			ready, err := f.p.observe(ack, false)
			results <- observation{ready, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	readyCount := 0
	for result := range results {
		if result.ready && result.err == nil {
			readyCount++
		} else if result.ready || !errors.Is(result.err, ErrProtocol) {
			t.Errorf("duplicate acknowledgement: ready=%v, err=%v", result.ready, result.err)
		}
	}
	if readyCount != 1 {
		t.Fatalf("readiness emitted %d times; want exactly once", readyCount)
	}
}
