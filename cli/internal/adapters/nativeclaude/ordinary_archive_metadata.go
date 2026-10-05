package nativeclaude

import (
	"bytes"
	"encoding/json"
	"math"
)

// These fields were observed in the 2.1.287 SDK recorder. They describe
// ingress and rendering, not permission decisions. Keep reference-only flags
// off real questions/results; never silently remove them to make readback pass.
func ordinaryArchiveProvenance(m map[string]json.RawMessage, kind string, phase int) error {
	if raw, ok := m["entrypoint"]; ok && !ordinaryLiteral(raw, "sdk-cli") {
		return ErrUnsupported
	}
	for _, key := range []string{"origin", "queueSkipAttachments", "queueTranscriptOnly"} {
		if _, ok := m[key]; ok && (phase != 0 || kind != "user") {
			return ErrUnsupported
		}
	}
	if m["origin"] != nil || m["queueSkipAttachments"] != nil || m["queueTranscriptOnly"] != nil {
		origin, err := object(m["origin"])
		if err != nil || !exchangeKeys(origin, "kind") || !ordinaryLiteral(origin["kind"], "unclassified") || string(m["queueSkipAttachments"]) != "true" || string(m["queueTranscriptOnly"]) != "true" {
			return ErrUnsupported
		}
	}
	for _, key := range []string{"promptSource", "turnOrigin", "turnPosition"} {
		if _, ok := m[key]; ok && (phase != 1 || kind != "user") {
			return ErrUnsupported
		}
	}
	if m["promptSource"] != nil || m["turnOrigin"] != nil || m["turnPosition"] != nil {
		position, err := object(m["turnPosition"])
		if err != nil || !exchangeKeys(position, "promptIndex", "turnIndex") || !ordinaryLiteral(m["promptSource"], "sdk") || !ordinaryLiteral(m["turnOrigin"], "sdk") || string(position["promptIndex"]) != "0" || string(position["turnIndex"]) != "1" {
			return ErrUnsupported
		}
	}
	for _, key := range []string{"apiBlockIndex", "effort", "perTurnEffort"} {
		if raw, ok := m[key]; ok {
			if kind != "assistant" {
				return ErrUnsupported
			}
			if key != "apiBlockIndex" {
				if value, err := exchangeString(raw, 64); err != nil || value == "" {
					return ErrProtocol
				}
			}
		}
	}
	if kind == "attachment" {
		for _, key := range []string{"message", "sourceToolAssistantUUID", "toolUseResult"} {
			if _, ok := m[key]; ok {
				return ErrUnsupported
			}
		}
	} else {
		for _, key := range []string{"attachment", "rendered", "renderedRole"} {
			if _, ok := m[key]; ok {
				return ErrUnsupported
			}
		}
	}
	return nil
}

func ordinaryLiteral(raw json.RawMessage, want string) bool {
	value, err := exchangeString(raw, len(want))
	return err == nil && value == want
}

// The SDK omits native indices. Pinned 2.1.287 merges same-message blocks in
// stable index order; fallback and expanded blocks can share one scalar index.
// Require that this replay order preserves the observed SDK record order,
// without inventing original indices from sibling counts or content lengths.
// Both supported native producers save this scalar. Unindexed replay can
// reorder tool/text blocks, so missing indices and per-block arrays are outside
// this contract instead of being silently treated as index zero.
func checkOrdinaryArchiveBlockOrder(row map[string]json.RawMessage, messageID string, seen map[string]float64) error {
	n, err := exchangeNumber(row["apiBlockIndex"], 63)
	if err != nil || n != math.Trunc(n) {
		return ErrProtocol
	}
	if previous, ok := seen[messageID]; ok && n < previous {
		return ErrProtocol
	}
	seen[messageID] = n
	return nil
}

// Native attachments contain model-visible instructions and tool schemas.
// Readback classifies and hashes them as native-added overhead; it does not
// claim CXT admitted their exact token count or grant the listed tools access.
// The full immutable archive is rechecked immediately before native resume.
func validateOrdinaryAttachment(row map[string]json.RawMessage, sessionID, cwd, model string) (string, error) {
	raw := row["attachment"]
	if len(raw) > exchangeAttachmentBytes {
		return "", ErrProtocol
	}
	m, err := object(raw)
	if err != nil {
		return "", err
	}
	kind, err := stringField(m, "type")
	if err != nil {
		return "", err
	}
	if rendered, ok := row["rendered"]; ok {
		if len(rendered) > exchangeAttachmentBytes || !ordinaryLiteral(row["renderedRole"], "system") {
			return "", ErrProtocol
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(rendered, &blocks) != nil || len(blocks) == 0 || len(blocks) > 64 {
			return "", ErrProtocol
		}
		for _, block := range blocks {
			if !exchangeKeys(block, "content") {
				return "", ErrUnsupported
			}
			if _, err := exchangeString(block["content"], exchangeAttachmentBytes); err != nil {
				return "", err
			}
		}
	} else if _, ok := row["renderedRole"]; ok {
		return "", ErrProtocol
	}
	switch kind {
	case "session_context", "date":
		return validateExchangeAttachment(raw, sessionID)
	case "environment":
		if !exchangeKeys(m, "type", "snapshot") {
			return "", ErrUnsupported
		}
		snapshot, err := object(m["snapshot"])
		if err != nil || !exchangeKeys(snapshot, "workingDirectory", "isWorktree", "isGitRepo", "additionalWorkingDirectories", "platform", "shell", "osVersion") || !ordinaryLiteral(snapshot["workingDirectory"], cwd) {
			return "", ErrProtocol
		}
		for _, key := range []string{"isWorktree", "isGitRepo"} {
			if _, err := exchangeBool(snapshot[key]); err != nil {
				return "", err
			}
		}
		for _, key := range []string{"platform", "shell", "osVersion"} {
			if _, err := exchangeString(snapshot[key], exchangeMetadataText); err != nil {
				return "", err
			}
		}
		if err := ordinaryTextArray(snapshot["additionalWorkingDirectories"], 64, 32<<10); err != nil {
			return "", err
		}
	case "model":
		if !exchangeKeys(m, "type", "identity", "text") {
			return "", ErrUnsupported
		}
		identity, err := object(m["identity"])
		if err != nil || !exchangeKeys(identity, "modelId", "marketingName", "knowledgeCutoff") || !ordinaryLiteral(identity["modelId"], model) {
			return "", ErrProtocol
		}
		for _, key := range []string{"marketingName", "knowledgeCutoff"} {
			if _, ok := identity[key]; ok {
				if _, err := exchangeString(identity[key], exchangeMetadataText); err != nil {
					return "", err
				}
			}
		}
		if _, err := exchangeString(m["text"], exchangeContextValue); err != nil {
			return "", err
		}
	case "total_tokens_reminder":
		// This is native prompt text, NOT the model's context-window size.
		if !exchangeKeys(m, "type", "text") {
			return "", ErrUnsupported
		}
		if _, err := exchangeString(m["text"], exchangeMetadataText); err != nil {
			return "", err
		}
	case "prompt_snapshot":
		if err := validateOrdinaryPromptSnapshot(m); err != nil {
			return "", err
		}
	default:
		return "", ErrUnsupported
	}
	return kind, nil
}

func ordinaryTextArray(raw json.RawMessage, count, textBytes int) error {
	var values []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &values) != nil || len(values) > count {
		return ErrProtocol
	}
	for _, value := range values {
		if _, err := exchangeString(value, textBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateOrdinaryPromptSnapshot(m map[string]json.RawMessage) error {
	if !exchangeKeys(m, "type", "systemPrompt", "tools", "cliPrefix", "reminderFold", "systemTurns", "toolChangeHeader", "inlineTools", "keptReminders", "echoWireToolInputs", "contextRendering") {
		return ErrUnsupported
	}
	if err := ordinaryTextArray(m["systemPrompt"], 128, exchangeAttachmentBytes); err != nil {
		return err
	}
	for _, key := range []string{"reminderFold", "inlineTools", "keptReminders", "echoWireToolInputs", "systemTurns", "toolChangeHeader"} {
		if raw, ok := m[key]; ok {
			value, err := exchangeBool(raw)
			if err != nil || value != (key == "systemTurns" || key == "toolChangeHeader") {
				return ErrUnsupported
			}
		}
	}
	if !ordinaryLiteral(m["contextRendering"], "announced") {
		return ErrUnsupported
	}
	if raw, ok := m["cliPrefix"]; ok {
		if _, err := exchangeString(raw, exchangeContextValue); err != nil {
			return err
		}
	}
	if raw, ok := m["tools"]; ok {
		var tools []map[string]json.RawMessage
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &tools) != nil || len(tools) > 512 {
			return ErrProtocol
		}
		seen := map[string]bool{}
		for _, tool := range tools {
			name, err := exchangeString(tool["name"], 256)
			if err != nil || name == "" || seen[name] || !exchangeKeys(tool, "name", "description", "schema") {
				return ErrProtocol
			}
			seen[name] = true
			description, err := exchangeString(tool["description"], exchangeContextValue)
			if err != nil {
				return err
			}
			schema, err := object(tool["schema"])
			if err != nil || !exchangeKeys(schema, "name", "description", "input_schema") || !ordinaryLiteral(schema["name"], name) || !ordinaryLiteral(schema["description"], description) {
				return ErrProtocol
			}
			if _, err := object(schema["input_schema"]); err != nil {
				return err
			}
		}
	}
	return nil
}
