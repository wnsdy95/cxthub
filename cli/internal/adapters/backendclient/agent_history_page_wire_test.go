package backendclient

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Synthetic native records exercise the real capture codecs, not hand-mirrored
// Go structs. The same golden pages are verified after backend storage/range
// reads in app/doc_turns_wire_test.go.
const historyClaudeWire = `{"type":"user","sessionId":"history-claude","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":"check <>& \uD55C\uAD6D\uC5B4"}}
{"type":"assistant","sessionId":"history-claude","timestamp":"2026-09-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"summary","signature":"SIG"},{"type":"text","text":"inspect"},{"type":"tool_use","id":"c","name":"Bash","input":{"command":"printf '<>&'"}}]}}
{"type":"user","sessionId":"history-claude","timestamp":"2026-09-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"result"}],"is_error":true}]}}`

const historyCodexWire = `{"timestamp":"2026-09-01T00:00:00Z","type":"session_meta","payload":{"id":"history-codex","cwd":"/synthetic","model":"synthetic"}}
{"timestamp":"2026-09-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect <>& \uD55C\uAD6D\uC5B4"}]}}
{"timestamp":"2026-09-01T00:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"encrypted_content":"ENC"}}
{"timestamp":"2026-09-01T00:00:03Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"ls\"}","call_id":"f"}}
{"timestamp":"2026-09-01T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"f","output":"ok"}}
{"timestamp":"2026-09-01T00:00:05Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"patch","call_id":"p","status":"completed"}}
{"timestamp":"2026-09-01T00:00:06Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"p","output":"done"}}
{"timestamp":"2026-09-01T00:00:07Z","type":"compacted","payload":{"replacement_history":[]}}`

const historyUnionWire = `[
 {"kind":"turn","role":"user","seq":0,"provider_metadata":{"turn_id":"t","create_time":9007199254740993}},
 {"kind":"message","role":"user","seq":1,"blocks":[{"type":"text","text":"<>&\u2028\u2029 \uD55C\uAD6D\uC5B4"}]},
 {"kind":"message","role":"assistant","seq":2,"blocks":[],"agent_message":false},
 {"kind":"message","role":"assistant","seq":3,"blocks":[],"agent_message":true,"agent_author":"","agent_recipient":"","locked":{"provider":"codex","scheme":"opaque","blob":"state"}},
 {"kind":"tool_call","seq":4,"call_id":"empty","tool_name":"read","input":{}},
 {"kind":"tool_result","seq":5,"call_id":"empty","output":"","is_error":false},
 {"kind":"tool_call","seq":6,"call_id":"object","tool_name":"read","provider_tool_name":"native","input":{"n":1.25,"items":[true,null,"<>&"]},"status":"completed"},
 {"kind":"tool_result","seq":7,"call_id":"object","output":{"n":1e30,"text":"<>&"},"is_error":true},
 {"kind":"tool_call","seq":8,"call_id":"array","tool_name":"read","input":{}},
 {"kind":"tool_result","seq":9,"call_id":"array","output":[{"type":"text","text":"array"}]},
 {"kind":"reasoning","seq":10,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"ENC"},"redacted_summary":"summary","cross_replayable":false},
 {"kind":"compaction","seq":11,"replacement":[],"replacement_complete":false},
 {"kind":"compaction","seq":12,"replacement":[{"kind":"message","role":"assistant","seq":0,"blocks":[],"agent_message":false}],"replacement_complete":true},
 {"kind":"compaction","seq":13,"locked":{"provider":"codex","scheme":"native","blob":"state"}},
 {"kind":"message","role":"assistant","seq":14,"blocks":[],"compact_summary":true}
]`

type agentHistoryWireFixture struct {
	Name string                  `json:"name"`
	Doc  domain.SessionDoc       `json:"doc"`
	Page domain.AgentHistoryPage `json:"page"`
}

func TestAgentHistoryWireParityWithRealCodecs(t *testing.T) {
	ctx := context.Background()
	claude, err := codec.NewClaudeCodec().Decode(ctx, []byte(historyClaudeWire))
	if err != nil {
		t.Fatal(err)
	}
	codex, err := codec.NewCodexCodec().Decode(ctx, []byte(historyCodexWire))
	if err != nil {
		t.Fatal(err)
	}
	union := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex, SessionOriginID: "all-unions"}}
	if err = json.Unmarshal([]byte(historyUnionWire), &union.Events); err != nil {
		t.Fatal(err)
	}
	var expected []agentHistoryWireFixture
	for _, item := range []struct {
		name string
		cir  domain.CIRDocument
	}{{"claude-codec", claude}, {"codex-codec", codex}, {"v2-union-presence", union}} {
		canonical, err := domain.CanonicalBytes(item.cir)
		if err != nil {
			t.Fatal(err)
		}
		doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: item.cir}
		wire, err := json.Marshal(item.cir.Events)
		if err != nil {
			t.Fatal(err)
		}
		page := domain.AgentHistoryPage{Version: 1, Hash: doc.Hash, Provider: item.cir.Envelope.SourceProvider, SessionID: item.cir.Envelope.SessionOriginID, Total: len(item.cir.Events), Before: len(item.cir.Events), NextBefore: -1, Turns: []domain.AgentHistoryTurn{{Start: 0, End: len(item.cir.Events), Hash: domain.HashContent(wire), Events: item.cir.Events}}}
		if err = domain.ValidateAgentHistoryPage(doc.Hash, domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20}, page); err != nil {
			t.Fatal(item.name, err)
		}
		expected = append(expected, agentHistoryWireFixture{Name: item.name, Doc: doc, Page: page})
	}
	const path = "../../../../schemas/testdata/agent-history-page-v1.json"
	want, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_AGENT_HISTORY_FIXTURE") == "1" {
		if err = os.WriteFile(path, append(want, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []agentHistoryWireFixture
	if err = json.Unmarshal(stored, &got); err != nil {
		t.Fatal(err)
	}
	// JSON round-trip both sides so map numeric types and optional false fields
	// are assessed exactly as they travel through BackendClient.
	var normalized []agentHistoryWireFixture
	if err = json.Unmarshal(want, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, normalized) {
		t.Fatal("real codec output differs from shared backend/CLI wire fixture")
	}
	for _, f := range got {
		if err = domain.ValidateAgentHistoryPage(f.Doc.Hash, domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20}, f.Page); err != nil {
			t.Fatal(f.Name, err)
		}
		wire, err := json.Marshal(f.Page)
		if err != nil {
			t.Fatal(err)
		}
		if err = verifyHistoryEventWire(wire, f.Page); err != nil {
			t.Fatal(f.Name, err)
		}
	}
}
