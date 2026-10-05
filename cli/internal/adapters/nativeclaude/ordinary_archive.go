package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
)

// Readback follows the observed root conversation, including tool-result
// parent branches. It never repairs an archive or treats body equality as
// permission to ignore behavior-changing metadata. Specialized native records
// remain explicit unsupported forms, independently of successful model output.
func (s *Session) verifyOrdinaryArchive(ctx context.Context, path string, q *firstQuestionState, reference ReferenceReceipt) (archiveVerification, error) {
	o := q.ordinary
	metadata := newExchangeMetadataValidator(s, q, reference)
	parent, phase, index, assistants, results := "", 0, 0, 0, 0
	seen := map[string]bool{}
	attachments := map[string]int{}
	blockOrder := map[string]float64{}
	var nativeAttachments []NativeArchiveAttachment
	verified, err := s.readArchive(ctx, path, func(m map[string]json.RawMessage) error {
		kind, err := stringField(m, "type")
		if err != nil {
			return err
		}
		switch kind {
		case "queue-operation", "atis-latch", "last-prompt", "cost-state":
			return metadata(m, kind)
		case "user", "assistant", "attachment":
		default:
			return ErrUnsupported
		}
		if !exchangeKeys(m, "type", "uuid", "parentUuid", "sessionId", "cwd", "isSidechain", "message", "attachment", "isMeta", "isApiErrorMessage", "isVisibleInTranscriptOnly", "isCompactSummary", "sourceToolAssistantUUID", "toolUseResult", "timestamp", "version", "gitBranch", "slug", "userType", "promptId", "permissionMode", "requestId", "entrypoint", "origin", "queueSkipAttachments", "queueTranscriptOnly", "promptSource", "turnOrigin", "turnPosition", "apiBlockIndex", "effort", "perTurnEffort", "rendered", "renderedRole") {
			return ErrUnsupported
		}
		if err := ordinaryArchiveProvenance(m, kind, phase); err != nil {
			return err
		}
		id, err := stringField(m, "uuid")
		if err != nil || seen[id] {
			return ErrProtocol
		}
		sid, err := stringField(m, "sessionId")
		if err != nil || sid != s.id {
			return ErrProtocol
		}
		cwd, err := exchangeString(m["cwd"], 32<<10)
		if err != nil || cwd != s.cwd {
			return ErrProtocol
		}
		side, err := boolean(m, "isSidechain")
		if err != nil || side {
			return ErrUnsupported
		}
		for _, key := range []string{"isApiErrorMessage", "isVisibleInTranscriptOnly", "isCompactSummary"} {
			if raw, ok := m[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
				return ErrUnsupported
			}
		}
		if phase != 0 {
			if err := ordinaryBehavior(m); err != nil {
				return err
			}
		}
		actualParent := ""
		if parent == "" {
			if !bytes.Equal(bytes.TrimSpace(m["parentUuid"]), []byte("null")) {
				return ErrProtocol
			}
		} else {
			actualParent, err = stringField(m, "parentUuid")
			if err != nil || !seen[actualParent] {
				return ErrProtocol
			}
		}
		seen[id] = true
		if phase < 2 {
			if kind != "user" || actualParent != parent {
				return ErrProtocol
			}
			text, err := referenceText(m["message"])
			if err != nil {
				return err
			}
			message, err := object(m["message"])
			if err != nil || !exchangeKeys(message, "role", "content") {
				return ErrUnsupported
			}
			if phase == 0 {
				meta, err := boolean(m, "isMeta")
				if err != nil || !meta || id != reference.MessageID || len(text) != reference.NativeUTF8Bytes || hashText(text) != reference.NativeContentHash {
					return ErrProtocol
				}
			} else if id != q.id || hashText(text) != o.questionHash || len(text) != o.questionBytes {
				return ErrProtocol
			}
			if _, ok := m["sourceToolAssistantUUID"]; ok {
				return ErrProtocol
			}
			if _, ok := m["toolUseResult"]; ok {
				return ErrProtocol
			}
			phase++
		} else if kind == "attachment" {
			// Native announcements occur before an API round, including after
			// a tool result. They cannot split assistant blocks or follow the
			// final answer, and never substitute for an observed wire record.
			if actualParent != parent || index >= len(o.records) || o.records[index].kind != "assistant" || (index > 0 && o.records[index-1].kind != "user") {
				return ErrUnsupported
			}
			kind, err := validateOrdinaryAttachment(m, s.id, s.cwd, q.summary.Model)
			previous, duplicate := attachments[kind]
			if err != nil || len(nativeAttachments) >= exchangeMetadataRows || (duplicate && previous == index) {
				return ErrUnsupported
			}
			if index != 0 && kind != "prompt_snapshot" && kind != "total_tokens_reminder" {
				return ErrUnsupported
			}
			attachments[kind] = index
			h, err := archiveContentHash(m["attachment"])
			if err != nil {
				return err
			}
			nativeAttachments = append(nativeAttachments, NativeArchiveAttachment{Type: kind, ContentHash: h, JSONBytes: len(m["attachment"])})
		} else {
			if index >= len(o.records) {
				return ErrProtocol
			}
			r := o.records[index]
			if r.id != id || r.kind != kind {
				return ErrProtocol
			}
			message, err := object(m["message"])
			if err != nil {
				return err
			}
			role, err := stringField(message, "role")
			if err != nil || role != kind {
				return ErrProtocol
			}
			h, err := archiveContentHash(message["content"])
			if err != nil || h != r.contentHash {
				return ErrProtocol
			}
			if kind == "assistant" {
				if err := checkOrdinaryArchiveBlockOrder(m, r.messageID, blockOrder); err != nil {
					return err
				}
				stop := "end_turn"
				for _, tool := range o.tools {
					if tool.messageID == r.messageID {
						stop = "tool_use"
					}
				}
				if r.terminalStop != "" && r.terminalStop != stop {
					return ErrProtocol
				}
				var output int64
				h, output, err = ordinaryAssistantProjection(message, stop)
				if output < r.outputTokens {
					return ErrProtocol
				}
			} else {
				h, err = archiveContentHash(m["message"])
			}
			if err != nil || h != r.messageHash {
				return ErrProtocol
			}
			if kind == "assistant" {
				if actualParent != parent {
					return ErrProtocol
				}
				mid, err := stringField(message, "id")
				if err != nil || mid != r.messageID {
					return ErrProtocol
				}
				if _, _, _, err := ordinaryAssistant(message, q.summary.Model); err != nil {
					return err
				}
				if _, ok := m["sourceToolAssistantUUID"]; ok {
					return ErrProtocol
				}
				if _, ok := m["toolUseResult"]; ok {
					return ErrProtocol
				}
				assistants++
			} else {
				// Native explicitly parents tool-result users to their producing
				// assistant, including a sibling in the same API message group.
				parentOK := false
				for _, toolID := range r.toolIDs {
					tool := o.tools[toolID]
					if tool == nil || !seen[tool.assistant] {
						return ErrProtocol
					}
					// Wire splitting can give a tool block and its native source
					// sibling distinct UUIDs within one API message.
					for _, candidate := range o.records {
						if candidate.id == actualParent && candidate.kind == "assistant" && candidate.messageID == tool.messageID {
							parentOK = true
						}
					}
				}
				if !parentOK {
					return ErrProtocol
				}
				if _, ok := m["sourceToolAssistantUUID"]; ok {
					source, err := stringField(m, "sourceToolAssistantUUID")
					if err != nil || source != actualParent {
						return ErrProtocol
					}
				}
				if raw, ok := m["toolUseResult"]; ok {
					h, err := archiveContentHash(raw)
					if err != nil || h != r.toolResultHash {
						return ErrProtocol
					}
				} else if r.toolResultHash != "" {
					return ErrProtocol
				}
				results++
			}
			index++
		}
		parent = id
		return nil
	})
	if err != nil {
		return archiveVerification{}, err
	}
	if phase != 2 || index != len(o.records) || assistants == 0 || !o.settled() {
		return archiveVerification{}, ErrProtocol
	}
	reference.Persisted = true
	verified.receipt = reference
	verified.exchange = &ExchangeArchiveReceipt{Reference: reference, QuestionID: q.id, QuestionHash: q.hash, NativeQuestionHash: o.questionHash, AnswerHash: hashText(q.answer), AssistantRecords: assistants, ToolResultRecords: results, NativeAttachments: nativeAttachments, Persisted: true}
	return verified, nil
}
