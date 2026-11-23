package codec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func codexTestItem(turn string, at int64, fields map[string]any) CodexItem {
	raw, _ := json.Marshal(fields)
	return CodexItem{TurnID: turn, CreatedAtMs: at, JSON: raw}
}

// Each kind of item Codex publishes becomes a message, a reasoning note, or
// a tool call with its result.
func TestCodexItemsBecomeEventsByTheirPublishedShape(t *testing.T) {
	items := []CodexItem{
		codexTestItem("t1", 1000, map[string]any{"type": "userMessage", "id": "u1", "content": []any{
			map[string]any{"type": "text", "text": "Add the parser.", "text_elements": []any{}},
			map[string]any{"type": "localImage", "path": "shot.png"},
			map[string]any{"type": "image", "detail": "high"},
		}}),
		codexTestItem("t1", 1001, map[string]any{"type": "hookPrompt", "id": "h1", "fragments": []any{map[string]any{"hookRunId": "r", "text": "Follow the house rules."}}}),
		codexTestItem("t1", 1002, map[string]any{"type": "reasoning", "id": "r1", "summary": []any{"Plan the parser."}, "content": []any{"It reads TOML."}}),
		codexTestItem("t1", 1003, map[string]any{"type": "plan", "id": "p1", "text": "1. parse 2. test"}),
		codexTestItem("t1", 1004, map[string]any{"type": "commandExecution", "id": "c1", "command": "git commit -m parser", "cwd": "/home/dev/app", "processId": "77",
			"aggregatedOutput": "[main abc1234] parser", "exitCode": 0, "durationMs": 12, "status": "completed", "commandActions": []any{}, "source": "agent"}),
		codexTestItem("t1", 1005, map[string]any{"type": "commandExecution", "id": "c2", "command": "false", "cwd": "/home/dev/app", "aggregatedOutput": "", "exitCode": 1, "status": "failed"}),
		codexTestItem("t1", 1006, map[string]any{"type": "commandExecution", "id": "c3", "command": "sleep 100", "aggregatedOutput": nil, "exitCode": nil, "status": "inProgress"}),
		codexTestItem("t1", 1007, map[string]any{"type": "fileChange", "id": "f1", "status": "completed", "changes": []any{map[string]any{"path": "parser.go", "kind": "add"}}}),
		codexTestItem("t1", 1008, map[string]any{"type": "mcpToolCall", "id": "m1", "server": "docs", "tool": "search", "status": "completed",
			"arguments": map[string]any{"q": "toml"}, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "found"}}}, "error": nil}),
		codexTestItem("t1", 1009, map[string]any{"type": "mcpToolCall", "id": "m2", "server": "docs", "tool": "search", "status": "failed", "arguments": "not an object", "error": map[string]any{"message": "down"}}),
		codexTestItem("t1", 1010, map[string]any{"type": "dynamicToolCall", "id": "d1", "namespace": "ide", "tool": "open", "arguments": map[string]any{"file": "a"}, "success": false, "contentItems": []any{}}),
		codexTestItem("t1", 1011, map[string]any{"type": "functionCallOutput", "id": "o1", "name": "lookup", "namespace": nil, "output": "42"}),
		codexTestItem("t1", 1012, map[string]any{"type": "webSearch", "id": "w1", "query": "toml spec", "action": map[string]any{"type": "search"}, "results": nil}),
		codexTestItem("t1", 1013, map[string]any{"type": "imageView", "id": "i1", "path": "shot.png"}),
		codexTestItem("t1", 1014, map[string]any{"type": "collabAgentToolCall", "id": "a1", "tool": "spawnAgent", "prompt": "Write tests.", "senderThreadId": "lead", "receiverThreadIds": []any{"worker"}, "status": "completed"}),
		codexTestItem("t1", 1015, map[string]any{"type": "sleep", "id": "s1", "durationMs": 500}),
		codexTestItem("t1", 1016, map[string]any{"type": "imageGeneration", "id": "g1", "status": "completed", "revisedPrompt": "a parser", "savedPath": "/home/dev/out.png"}),
		codexTestItem("t1", 1017, map[string]any{"type": "enteredReviewMode", "id": "e1", "review": "the parser"}),
		codexTestItem("t1", 1018, map[string]any{"type": "contextCompaction", "id": "x1"}),
		codexTestItem("t1", 1019, map[string]any{"type": "agentMessage", "id": "g2", "text": "Committed the parser.", "phase": "final"}),
	}
	doc, err := DecodeCodexItems(items, domain.Envelope{}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Envelope.SourceProvider != domain.ProviderCodex || doc.Envelope.CIRVersion != domain.CIRVersionV1 || doc.Envelope.Fidelity != domain.FidelityFull {
		t.Fatalf("envelope: %+v", doc.Envelope)
	}
	var got []string
	for index, event := range doc.Events {
		if event.Seq != 7+index || event.ID == "" || event.Ts == "" {
			t.Fatalf("event %d is not numbered, named and dated: %+v", index, event)
		}
		line := event.Kind + " " + event.ID
		switch event.Kind {
		case domain.EventMessage:
			line += " " + event.Role + ": " + event.Blocks[0].Text
		case domain.EventReasoning:
			line += ": " + event.RedactedSummary
		case domain.EventToolCall:
			input, _ := json.Marshal(event.Input)
			line += " " + event.ToolName + "<" + event.ProviderToolName + "> " + event.Status + " " + string(input)
		case domain.EventToolResult:
			output, _ := json.Marshal(event.Output)
			line += " " + string(output)
			if event.IsError {
				line += " error"
			}
		}
		got = append(got, line)
	}
	want := []string{
		"message u1 user: Add the parser.\n[localImage shot.png]\n[image]",
		"message h1 system: Follow the house rules.",
		"reasoning r1: Plan the parser.\nIt reads TOML.",
		"message p1 assistant: 1. parse 2. test",
		`tool_call c1 shell<exec_command> completed {"actions":[],"command":"git commit -m parser"}`,
		`tool_result c1 "[main abc1234] parser"`,
		`tool_call c2 shell<exec_command> failed {"command":"false"}`,
		`tool_result c2 "" error`,
		`tool_call c3 shell<exec_command> inProgress {"command":"sleep 100"}`,
		`tool_call f1 apply_patch<apply_patch> completed {"changes":[{"kind":"add","path":"parser.go"}]}`,
		`tool_result f1 "completed"`,
		`tool_call m1 mcp:docs:search<mcp__docs__search> completed {"q":"toml"}`,
		`tool_result m1 {"content":[{"text":"found","type":"text"}]}`,
		`tool_call m2 mcp:docs:search<mcp__docs__search> failed {"arguments":"not an object"}`,
		`tool_result m2 {"message":"down"} error`,
		`tool_call d1 unknown:ide.open<ide.open>  {"file":"a"}`,
		`tool_result d1 [] error`,
		`tool_call o1 unknown:lookup<lookup>  {}`,
		`tool_result o1 "42"`,
		`tool_call w1 web_search<web_search>  {"action":{"type":"search"},"query":"toml spec"}`,
		`tool_call i1 read_file<view_image>  {"path":"shot.png"}`,
		`tool_call a1 unknown:collabAgentToolCall<collabAgentToolCall> completed {"prompt":"Write tests.","receiverThreadIds":["worker"],"senderThreadId":"lead","tool":"spawnAgent"}`,
		`tool_call s1 unknown:sleep<sleep>  {"durationMs":500}`,
		`tool_call g1 unknown:imageGeneration<imageGeneration> completed {"revisedPrompt":"a parser"}`,
		"message e1 system: enteredReviewMode: the parser",
		"message x1 system: Codex compacted the conversation here.",
		"message g2 assistant: Committed the parser.",
	}
	if !reflect.DeepEqual(got, want) {
		for index := range want {
			if index >= len(got) || got[index] != want[index] {
				gotLine := "(nothing)"
				if index < len(got) {
					gotLine = got[index]
				}
				t.Fatalf("event %d:\n got %s\nwant %s", index, gotLine, want[index])
			}
		}
		t.Fatalf("%d events, want %d: %v", len(got), len(want), got[len(want):])
	}
	// Where on this computer a command ran, and which process ran it, is not
	// part of what is converted.
	whole, _ := json.Marshal(doc.Events)
	for _, local := range []string{"/home/dev", `"processId"`, `"cwd"`, "savedPath"} {
		if strings.Contains(string(whole), local) {
			t.Fatalf("the events hold %s", local)
		}
	}
}

// A kind of item this code was not written for is carried whole, and a known
// kind whose fields changed is read as far as it can be: a newer Codex does
// not stop the reading.
func TestCodexItemsOfAnotherVersionAreStillRead(t *testing.T) {
	items := []CodexItem{
		codexTestItem("t1", 0, map[string]any{"type": "holoDeck", "id": "n1", "scene": "bridge", "cwd": "kept: not known to be a path here", "status": "running", "nested": map[string]any{"a": []any{1, 2}}}),
		codexTestItem("t1", 0, map[string]any{"type": "agentMessage", "id": "n2", "text": map[string]any{"parts": []any{"moved"}}}),
		codexTestItem("t1", 0, map[string]any{"type": "userMessage", "id": "n3", "content": "plain text in a newer shape"}),
		codexTestItem("t1", 0, map[string]any{"type": "commandExecution", "id": "n4", "command": []any{"git", "status"}, "exitCode": "zero"}),
		codexTestItem("t1", 0, map[string]any{"type": "reasoning", "id": "n5"}),
	}
	doc, err := DecodeCodexItems(items, domain.Envelope{SourceModel: "kept"}, 0)
	if err != nil || len(doc.Events) != 5 || doc.Envelope.SourceModel != "kept" {
		t.Fatalf("events: %+v %v", doc.Events, err)
	}
	unknown := doc.Events[0]
	if unknown.Kind != domain.EventToolCall || unknown.ToolName != "unknown:holoDeck" || unknown.ProviderToolName != "holoDeck" || unknown.Status != "running" || unknown.Ts != "" ||
		!reflect.DeepEqual(unknown.Input, map[string]interface{}{"scene": "bridge", "cwd": "kept: not known to be a path here", "nested": map[string]interface{}{"a": []interface{}{float64(1), float64(2)}}}) {
		t.Fatalf("an unknown item: %+v", unknown)
	}
	if doc.Events[1].Kind != domain.EventMessage || doc.Events[1].Blocks[0].Text != "" {
		t.Fatalf("a message whose text moved: %+v", doc.Events[1])
	}
	if doc.Events[2].Blocks[0].Text != "plain text in a newer shape" {
		t.Fatalf("a user message as plain text: %+v", doc.Events[2])
	}
	if command := doc.Events[3]; command.Kind != domain.EventToolCall || !reflect.DeepEqual(command.Input["command"], []interface{}{"git", "status"}) {
		t.Fatalf("a command in another shape: %+v", command)
	}
	if doc.Events[4].Kind != domain.EventReasoning || doc.Events[4].RedactedSummary != "" {
		t.Fatalf("reasoning without text: %+v", doc.Events[4])
	}
	if CodexItemTypeKnown("holoDeck") || !CodexItemTypeKnown("agentMessage") {
		t.Fatal("which types are known")
	}

	// What is not an item at all is refused.
	for name, raw := range map[string]string{"not JSON": `{"type":`, "not an object": `["agentMessage"]`, "no type": `{"id":"x"}`} {
		if _, err := DecodeCodexItems([]CodexItem{{JSON: json.RawMessage(raw)}}, domain.Envelope{}, 0); err == nil {
			t.Fatalf("%s was converted", name)
		}
	}
	if empty, err := DecodeCodexItems(nil, domain.Envelope{}, 3); err != nil || len(empty.Events) != 0 {
		t.Fatalf("no items: %+v %v", empty, err)
	}
}
