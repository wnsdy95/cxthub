package session

import (
	"bytes"
	"encoding/json"
	"regexp"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// id-rewrite on materialize — distills the internal session id of the materialized version into a file/restart id.
// The agent's resume uses the session id to find the session (codex: session_meta.id, claude: record sessionId), so if the original session id remains, the materialized version will not restart or (if codex resume <newid> is not found) will conflict with the original live session.

// rewriteCodexSessionID rewrites the session_meta.payload.id to id.
// Only the session_meta row is parsed and re-serialized; other rows are preserved as bytes.
func rewriteCodexSessionID(raw []byte, id string) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		if !bytes.Contains(line, []byte(`"session_meta"`)) {
			continue
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal(line, &rec) != nil || string(rec["type"]) != `"session_meta"` {
			continue
		}
		var payload map[string]interface{}
		if json.Unmarshal(rec["payload"], &payload) != nil {
			continue
		}
		payload["id"] = id
		if _, ok := payload["session_id"]; ok {
			payload["session_id"] = id // schema — must maintain id pair for resume to find it
		}
		pb, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		rec["payload"] = pb
		lb, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		lines[i] = lb
	}
	return bytes.Join(lines, []byte("\n"))
}

var claudeSessionIDRe = regexp.MustCompile(`("sessionId"\s*:\s*")[^"]*(")`)

// rewriteClaudeSessionID rewrites the sessionId field of each transcript record to id.
// Only the field value is replaced; other bytes are preserved (UUIDs in conversation text are not altered).
func rewriteClaudeSessionID(raw []byte, id string) []byte {
	return claudeSessionIDRe.ReplaceAll(raw, []byte("${1}"+id+"${2}"))
}

// nativizeClaudeSession converts the materialized version into a native form that Claude can actually resume and render: sessionId rewrite + message records with uuid/parentUuid chain.
//
// Claude restores the transcript using a UUID chain (following parentUuid backward from the last record) — if there is no chain, resume will treat the conversation as empty and not draw anything in the context window, and the model will not receive history (empirically verified: resuming a chainless materialized version starts a new chain with parentUuid:null as the first native record).
//
// Preservation Contract:
//   - If any message record already has a UUID (native original, etc.), the chain already exists, so only sessionId replacement is performed without destroying the existing tree (sidechain branching, summary leafUuid reference).
//   - When assigning the chain, only message (type user/assistant) records are re-serialized, and other rows are preserved as bytes (sessionId is replaced using regex — key order and numeric precision unchanged).
//   - Rows that cannot be parsed are left as is (fail-open).
func nativizeClaudeSession(raw []byte, id string) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	// 1-pass: existing chain existence check — if a UUID exists in a message record, only perform id replacement.
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.Contains(trimmed, []byte(`"uuid"`)) {
			continue
		}
		var rec struct {
			Type string      `json:"type"`
			UUID interface{} `json:"uuid"`
		}
		if json.Unmarshal(trimmed, &rec) == nil && (rec.Type == "user" || rec.Type == "assistant") && rec.UUID != nil {
			return rewriteClaudeSessionID(raw, id)
		}
	}
	prev := ""
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var rec map[string]interface{}
		if json.Unmarshal(trimmed, &rec) != nil {
			lines[i] = claudeSessionIDRe.ReplaceAll(line, []byte("${1}"+id+"${2}"))
			continue
		}
		t, _ := rec["type"].(string)
		if t != "user" && t != "assistant" {
			// Non-message rows are preserved as bytes (serialization forbidden — maintain int64 precision and key order).
			lines[i] = claudeSessionIDRe.ReplaceAll(line, []byte("${1}"+id+"${2}"))
			continue
		}
		if _, ok := rec["sessionId"]; ok {
			rec["sessionId"] = id
		}
		u := providerfs.NewSessionID()
		rec["uuid"] = u
		if prev == "" {
			rec["parentUuid"] = nil
		} else {
			rec["parentUuid"] = prev
		}
		if _, ok := rec["isSidechain"]; !ok {
			rec["isSidechain"] = false
		}
		if t == "user" {
			if _, ok := rec["userType"]; !ok {
				rec["userType"] = "external"
			}
		}
		prev = u
		if lb, err := json.Marshal(rec); err == nil {
			lines[i] = lb
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
