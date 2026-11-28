package codec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// codex_items.go converts the items of a Codex thread into CIR events.
//
// An item is the unit Codex itself gives other programs: its app-server
// answers thread/items/list with them, and its thread history database keeps
// the same JSON in thread_items.item_json. Their shape is published by
// `codex app-server generate-json-schema` (ThreadItem). The conversion is
// written against that published shape and not against where the items are
// kept, so the same code serves either place.
//
// Two rules keep a newer Codex from breaking it:
//   - an item of a type this code does not know is carried whole, as a tool
//     call named after the type with the item as its input;
//   - a known item is read field by field, and a field that is absent or has
//     another type than expected is read as empty, never as an error.

// CodexItem is one item of a thread with what its place in the thread adds:
// the turn it belongs to and when it was created.
type CodexItem struct {
	TurnID      string
	CreatedAtMs int64
	// JSON is the item as Codex gives it: an object with "type" and "id".
	JSON json.RawMessage
}

// codexItemKnown lists the item types the conversion reads by their fields.
var codexItemKnown = map[string]struct{}{
	"userMessage": {}, "hookPrompt": {}, "agentMessage": {}, "functionCallOutput": {}, "plan": {}, "reasoning": {},
	"commandExecution": {}, "fileChange": {}, "mcpToolCall": {}, "dynamicToolCall": {}, "collabAgentToolCall": {},
	"subAgentActivity": {}, "webSearch": {}, "imageView": {}, "sleep": {}, "imageGeneration": {},
	"enteredReviewMode": {}, "exitedReviewMode": {}, "contextCompaction": {},
}

// codexItemLocal names fields that say where on this computer something is
// or which process ran it. They are left out of what is uploaded.
var codexItemLocal = map[string]struct{}{"cwd": {}, "processId": {}, "scriptPath": {}, "savedPath": {}}

// DecodeCodexItemsCounted converts items, in the order given, into events
// numbered from start, and says how many events each item gave, in order.
// previous is the envelope of the events already converted for the same
// thread. An item is converted on its own, so the counts place every item in
// the events. An item that is not a JSON object with a type is an error: that
// is not an item at all.
func DecodeCodexItemsCounted(items []CodexItem, previous domain.Envelope, start int) (domain.CIRDocument, []int, error) {
	doc := domain.CIRDocument{Envelope: previous}
	doc.Envelope.SourceModels = append([]string(nil), previous.SourceModels...)
	doc.Envelope.CIRVersion = domain.CIRVersionV1
	doc.Envelope.SourceProvider = domain.ProviderCodex
	doc.Envelope.Fidelity = domain.FidelityFull
	counts := make([]int, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item.JSON, &fields); err != nil {
			return domain.CIRDocument{}, nil, fmt.Errorf("codex item: %w", err)
		}
		kind, id := codexString(fields["type"]), codexString(fields["id"])
		if kind == "" {
			return domain.CIRDocument{}, nil, fmt.Errorf("codex item %q has no type", id)
		}
		held := len(doc.Events)
		for _, event := range codexItemEvents(kind, id, fields) {
			event.Seq = start + len(doc.Events)
			if event.ID == "" {
				event.ID = id
			}
			if item.CreatedAtMs > 0 && event.Ts == "" {
				event.Ts = time.UnixMilli(item.CreatedAtMs).UTC().Format(time.RFC3339Nano)
			}
			doc.Events = append(doc.Events, event)
		}
		counts = append(counts, len(doc.Events)-held)
	}
	return doc, counts, nil
}

// codexItemEvents gives the events one item becomes: a message, a reasoning
// note, or a tool call followed by its result.
func codexItemEvents(kind, id string, fields map[string]json.RawMessage) []domain.Event {
	text := func(role string, parts ...string) []domain.Event {
		joined := strings.TrimSpace(strings.Join(parts, "\n"))
		return []domain.Event{{Kind: domain.EventMessage, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: joined}}}}
	}
	call := func(provider string, input map[string]interface{}, output interface{}, failed bool) []domain.Event {
		status := codexString(fields["status"])
		events := []domain.Event{{Kind: domain.EventToolCall, CallID: id, ToolName: canonicalToolName(provider), ProviderToolName: provider, Input: input, Status: status}}
		if output != nil {
			events = append(events, domain.Event{Kind: domain.EventToolResult, CallID: id, Output: normalizeToolResultOutput(output), IsError: failed})
		}
		return events
	}
	switch kind {
	case "userMessage":
		return text("user", codexUserInputs(fields["content"])...)
	case "agentMessage":
		return text("assistant", codexString(fields["text"]))
	case "hookPrompt":
		return text("system", codexFragments(fields["fragments"])...)
	case "plan":
		return text("assistant", codexString(fields["text"]))
	case "enteredReviewMode", "exitedReviewMode":
		return text("system", kind+": "+codexString(fields["review"]))
	case "contextCompaction":
		return text("system", "Codex compacted the conversation here.")
	case "reasoning":
		summary := strings.TrimSpace(strings.Join(append(codexStrings(fields["summary"]), codexStrings(fields["content"])...), "\n"))
		return []domain.Event{{Kind: domain.EventReasoning, RedactedSummary: summary, CrossReplayable: true}}
	case "commandExecution":
		input := map[string]interface{}{"command": codexValue(fields["command"])}
		if actions := codexValue(fields["commandActions"]); actions != nil {
			input["actions"] = actions
		}
		exit, hasExit := codexNumber(fields["exitCode"])
		output := codexString(fields["aggregatedOutput"])
		var result interface{}
		if _, present := fields["aggregatedOutput"]; present && fields["aggregatedOutput"] != nil && string(fields["aggregatedOutput"]) != "null" {
			result = output
		}
		return call("exec_command", input, result, hasExit && exit != 0)
	case "fileChange":
		return call("apply_patch", map[string]interface{}{"changes": codexValue(fields["changes"])}, codexString(fields["status"]), codexFailed(fields["status"]))
	case "mcpToolCall":
		server, tool := codexString(fields["server"]), codexString(fields["tool"])
		input, _ := codexValue(fields["arguments"]).(map[string]interface{})
		if input == nil {
			input = map[string]interface{}{"arguments": codexValue(fields["arguments"])}
		}
		output, failed := codexValue(fields["result"]), false
		if problem := codexValue(fields["error"]); problem != nil {
			output, failed = problem, true
		}
		return call("mcp__"+server+"__"+tool, input, output, failed)
	case "dynamicToolCall":
		input, _ := codexValue(fields["arguments"]).(map[string]interface{})
		if input == nil {
			input = map[string]interface{}{"arguments": codexValue(fields["arguments"])}
		}
		name := codexString(fields["tool"])
		if namespace := codexString(fields["namespace"]); namespace != "" {
			name = namespace + "." + name
		}
		success, known := codexBool(fields["success"])
		return call(name, input, codexValue(fields["contentItems"]), known && !success)
	case "functionCallOutput":
		name := codexString(fields["name"])
		if namespace := codexString(fields["namespace"]); namespace != "" {
			name = namespace + "." + name
		}
		return call(name, map[string]interface{}{}, codexValue(fields["output"]), false)
	case "webSearch":
		return call("web_search", map[string]interface{}{"query": codexString(fields["query"]), "action": codexValue(fields["action"])}, codexValue(fields["results"]), false)
	case "imageView":
		return call("view_image", map[string]interface{}{"path": codexString(fields["path"])}, nil, false)
	}
	// Every other item, known or not, is a tool call that carries its own
	// fields. A type this code does not know keeps everything it holds.
	input := map[string]interface{}{}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	_, known := codexItemKnown[kind]
	for _, name := range names {
		if name == "type" || name == "id" || name == "status" {
			continue
		}
		if _, local := codexItemLocal[name]; local && known {
			continue
		}
		input[name] = codexValue(fields[name])
	}
	return call(kind, input, nil, false)
}

// CodexItemTypeKnown reports whether the conversion reads items of that type
// by their fields. Any other type is carried whole.
func CodexItemTypeKnown(kind string) bool {
	_, known := codexItemKnown[kind]
	return known
}

func codexString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func codexStrings(raw json.RawMessage) []string {
	var values []string
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func codexNumber(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func codexBool(raw json.RawMessage) (bool, bool) {
	var value bool
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

// codexValue reads any JSON value; nothing, or null, is nil.
func codexValue(raw json.RawMessage) interface{} {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return decodeJSONValue(raw)
}

func codexFailed(raw json.RawMessage) bool {
	switch codexString(raw) {
	case "failed", "declined", "error":
		return true
	}
	return false
}

// codexUserInputs reads what a user message holds: text as it is, and a
// short mark for anything that is not text, so that it is not lost unseen.
func codexUserInputs(raw json.RawMessage) []string {
	var inputs []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &inputs) != nil {
		if plain := codexString(raw); plain != "" {
			return []string{plain}
		}
		return nil
	}
	var parts []string
	for _, input := range inputs {
		switch kind := codexString(input["type"]); kind {
		case "text":
			parts = append(parts, codexString(input["text"]))
		case "":
		default:
			label := "[" + kind
			for _, name := range []string{"name", "path", "url"} {
				if value := codexString(input[name]); value != "" && !strings.HasPrefix(value, "data:") {
					label += " " + value
					break
				}
			}
			parts = append(parts, label+"]")
		}
	}
	return parts
}

// codexFragments reads the text of a hook prompt's fragments.
func codexFragments(raw json.RawMessage) []string {
	var fragments []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fragments) != nil {
		return codexStrings(raw)
	}
	var parts []string
	for _, fragment := range fragments {
		if text := codexString(fragment["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return parts
}
