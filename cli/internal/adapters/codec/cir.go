// Package codec — cir.go: CIR-related internal utilities (shared logic for two codecs).
// Public interfaces are ClaudeCodec / CodexCodec; this file contains internal shared logic.
package codec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// normalizeCIR sorts CIR events in ascending order by seq. Used in the CanonicalBytes (hash) preprocessing step. Returns a sorted copy of the input slice without modifying it.
func normalizeCIR(doc domain.CIRDocument) domain.CIRDocument {
	out := doc
	events := make([]domain.Event, len(doc.Events))
	copy(events, doc.Events)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	out.Events = events
	return out
}

// assignSeq assigns consecutive seq numbers to the events slice in the order they appear (preserving the original order).
func assignSeq(events []domain.Event) []domain.Event {
	for i := range events {
		events[i].Seq = i
	}
	return events
}

// parseToolInput converts the provider's tool input string (function_call.arguments / custom_tool_call.input) to a CIR tool_call.Input (map). If parsed as a JSON object, it returns as is; otherwise, it wraps in {"_raw": s}. (To preserve non-JSON input without loss for apply_patch patch text.)
func parseToolInput(s string) map[string]interface{} {
	if s == "" {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err == nil && m != nil {
		return m
	}
	return map[string]interface{}{"_raw": s}
}

// marshalToolInput serializes a CIR tool_call.Input to a provider argument string. For {"_raw": s}, it restores the original string; otherwise, it serializes as a JSON object string.
func marshalToolInput(input map[string]interface{}) string {
	if input == nil {
		return "{}"
	}
	if raw, ok := input["_raw"]; ok && len(input) == 1 {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// decodeJSONValue deserializes raw JSON to interface{} (e.g., tool_result.output). Returns an empty string if raw is empty.
func decodeJSONValue(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// normalizeToolResultOutput narrows provider-specific tool_result values to CIR schema's string|object|array. It preserves the original shape of Claude's multi-modal content and Codex Responses' input_text/input_image/input_file arrays at the provider boundary. Scalars not allowed by CIR are promoted to JSON text (provider compatibility rules).
func normalizeToolResultOutput(v interface{}) interface{} {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]interface{}:
		return x
	case []interface{}:
		return x
	default:
		return toolResultText(x)
	}
}

// claudeToolResultContent narrows CIR output to Claude tool_result.content schema. It preserves the original shape of Claude's allowed block arrays, including images/documents. Mixed arrays with cross-provider elements or input_text/output_text rejected by Claude are flattened to plain text to prevent API 400. Objects and scalars not directly allowed by Claude are also flattened.
func claudeToolResultContent(v interface{}, preserveNativeBlocks bool) interface{} {
	if !preserveNativeBlocks {
		return toolResultText(v)
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.RawMessage:
		return claudeToolResultContent(decodeJSONValue(x), preserveNativeBlocks)
	case []interface{}:
		if isClaudeToolResultBlockArray(x) {
			return x
		}
		return toolResultText(x)
	default:
		return toolResultText(x)
	}
}

func isClaudeToolResultBlockArray(blocks []interface{}) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		m, ok := block.(map[string]interface{})
		if !ok {
			return false
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "text", "image", "document", "search_result", "tool_reference":
			// Claude API allowance block + Claude Code empirical tool_reference.
		default:
			return false
		}
	}
	return true
}

// codexToolResultOutput narrows the CIR output to the Responses API's
// function_call_output.output schema. The provider Codex allowance block array is
// preserved, while cross-provider arrays or past cxt leaked Claude tags are downgraded to plain text.
// The Responses API structured output is an array of input_text/input_image/input_file,
// and bare objects are not allowed, so it is sent as a JSON string.
func codexToolResultOutput(v interface{}, preserveNativeBlocks bool) interface{} {
	if !preserveNativeBlocks {
		return toolResultText(v)
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.RawMessage:
		return codexToolResultOutput(decodeJSONValue(x), preserveNativeBlocks)
	case []interface{}:
		if isCodexToolResultBlockArray(x) {
			return x
		}
		return toolResultText(x)
	default:
		return toolResultText(x)
	}
}

func isCodexToolResultBlockArray(blocks []interface{}) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		m, ok := block.(map[string]interface{})
		if !ok {
			return false
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "input_text", "input_image", "input_file":
			// Responses API function-call output allowed content union.
			// output_text is for assistant message output and is not part of this union,
			// so it is intentionally excluded. Unknown future/polluted tags can be resumed as plain text.
		default:
			return false
		}
	}
	return true
}

// toolResultText converts any CIR tool_result value to a provider-neutral plain text.
// It is called when downgrading incompatible arrays for the provider target to prevent
// reverse-provider-specific tags from being materialized in the result.
func toolResultText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.RawMessage:
		return toolResultText(decodeJSONValue(x))
	case []interface{}:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if text, ok := toolResultBlockText(item); ok {
				parts = append(parts, text)
				continue
			}
			parts = append(parts, toolResultText(item))
		}
		return strings.Join(parts, "\n\n")
	case map[string]interface{}:
		if text, ok := toolResultBlockText(x); ok {
			return text
		}
		return marshalToolResultText(x)
	default:
		// If a directly constructed CIR contains JSON-compatible Go types like []map[...],
		// it is converted to a general JSON value to apply the same rules.
		raw, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		var generic interface{}
		if err := json.Unmarshal(raw, &generic); err == nil {
			switch generic.(type) {
			case []interface{}, map[string]interface{}, string, nil:
				return toolResultText(generic)
			}
		}
		return string(raw)
	}
}

func toolResultBlockText(v interface{}) (string, bool) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return "", false
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "text", "input_text", "output_text", "summary_text":
		text, ok := m["text"].(string)
		return text, ok
	default:
		return "", false
	}
}

func marshalToolResultText(v interface{}) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
