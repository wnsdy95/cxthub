package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
)

// ExchangeArchiveReceipt proves post-close readback of the admitted reference,
// question and assistant content. It does not attest fsync, TUI readiness or a
// provider's capacity. Hidden assistant content is compared by hash, never
// included in this receipt.
type ExchangeArchiveReceipt struct {
	Reference          ReferenceReceipt          `json:"reference"`
	QuestionID         string                    `json:"question_id"`
	QuestionHash       string                    `json:"question_hash"`
	NativeQuestionHash string                    `json:"native_question_hash,omitempty"`
	ToolResultRecords  int                       `json:"tool_result_records,omitempty"`
	AnswerHash         string                    `json:"answer_hash"`
	AssistantRecords   int                       `json:"assistant_records"`
	NativeAttachments  []NativeArchiveAttachment `json:"native_attachments,omitempty"`
	Persisted          bool                      `json:"persisted"`
}

// NativeArchiveAttachment identifies native-added model context separately
// from CXT's reference and the admitted question. Shape/hash validation does
// not claim this content was included in the pre-query token measurement.
type NativeArchiveAttachment struct {
	Type        string `json:"type"`
	ContentHash string `json:"content_hash"`
	JSONBytes   int    `json:"json_bytes"`
}

type archiveAssistant struct{ id, contentHash string }

func (e *FirstExchange) VerifyArchive(ctx context.Context, path string) (ExchangeArchiveReceipt, error) {
	if e == nil || e.s == nil {
		return ExchangeArchiveReceipt{}, ErrState
	}
	verified, err := e.s.verifyExchangeArchive(ctx, path)
	if err != nil {
		return ExchangeArchiveReceipt{}, err
	}
	e.s.mu.Lock()
	e.s.verifiedArchive = &verified
	e.s.mu.Unlock()
	receipt := *verified.exchange
	receipt.NativeAttachments = append([]NativeArchiveAttachment(nil), receipt.NativeAttachments...)
	return receipt, nil
}

// PrepareIdleResume reuses the same frozen invocation and one-shot lifecycle as
// no-query resume, but requires a completed-exchange archive proof. No question
// is sent again and the native archive is never rewritten.
func (e *FirstExchange) PrepareIdleResume(ctx context.Context, path string) (*IdleResumePlan, error) {
	if e == nil || e.s == nil {
		return nil, ErrState
	}
	return e.s.prepareIdleResume(ctx, path, true)
}

func (s *Session) verifyResumeArchive(ctx context.Context, path string) (archiveVerification, error) {
	s.mu.Lock()
	queried := s.firstQuestion != nil
	s.mu.Unlock()
	if queried {
		return s.verifyExchangeArchive(ctx, path)
	}
	return s.verifyArchive(ctx, path)
}

func (s *Session) verifyExchangeArchive(ctx context.Context, path string) (archiveVerification, error) {
	if err := ctx.Err(); err != nil {
		return archiveVerification{}, err
	}
	select {
	case <-s.closed:
	default:
		return archiveVerification{}, ErrState
	}
	s.mu.Lock()
	q, reference := s.firstQuestion, s.receipt
	valid := s.version == "2.1.287" && s.closeErr == nil && s.err == nil && q != nil && q.completed && reference.NoTurnAcknowledged
	s.mu.Unlock()
	if !valid {
		return archiveVerification{}, ErrState
	}
	if q.ordinary != nil {
		return s.verifyOrdinaryArchive(ctx, path, q, reference)
	}
	// Protocol EOF and Close have completed: q is now immutable. Validate the
	// single path native resume will select, including intervening attachments.
	parent, phase, assistants := "", 0, 0
	seen := map[string]bool{}
	attachments := map[string]bool{}
	metadata := newExchangeMetadataValidator(s, q, reference)
	var nativeAttachments []NativeArchiveAttachment
	verified, err := s.readArchive(ctx, path, func(m map[string]json.RawMessage) error {
		kind, err := stringField(m, "type")
		if err != nil {
			return err
		}
		if compact, ok := m["isCompactSummary"]; ok && !bytes.Equal(bytes.TrimSpace(compact), []byte("false")) {
			return ErrProtocol
		}
		switch kind {
		case "queue-operation", "atis-latch", "last-prompt", "cost-state":
			return metadata(m, kind)
		case "user", "assistant", "attachment":
		default:
			return ErrProtocol
		}
		id, err := stringField(m, "uuid")
		if err != nil || seen[id] {
			return ErrProtocol
		}
		seen[id] = true
		sid, err := stringField(m, "sessionId")
		if err != nil || sid != s.id {
			return ErrProtocol
		}
		var cwd string
		if json.Unmarshal(m["cwd"], &cwd) != nil || cwd != s.cwd {
			return ErrProtocol
		}
		side, err := boolean(m, "isSidechain")
		if err != nil || side {
			return ErrProtocol
		}
		// These flags change native prompt/answer selection without changing
		// content. A matching text hash cannot attest a hidden or error record.
		for _, key := range []string{"isApiErrorMessage", "isVisibleInTranscriptOnly"} {
			if raw, ok := m[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
				return ErrProtocol
			}
		}
		if parent == "" {
			if !bytes.Equal(bytes.TrimSpace(m["parentUuid"]), []byte("null")) {
				return ErrProtocol
			}
		} else if p, err := stringField(m, "parentUuid"); err != nil || p != parent {
			return ErrProtocol
		}
		switch kind {
		case "user":
			text, err := referenceText(m["message"])
			if err != nil {
				return err
			}
			if phase == 0 {
				meta, err := boolean(m, "isMeta")
				if err != nil || !meta || id != reference.MessageID || len(text) != reference.NativeUTF8Bytes || hashText(text) != reference.NativeContentHash {
					return ErrProtocol
				}
			} else if phase == 1 {
				if id != q.id || len(text) != q.bytes || hashText(text) != q.hash {
					return ErrProtocol
				}
				if raw, ok := m["isMeta"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
					return ErrProtocol
				}
			} else {
				return ErrProtocol
			}
			phase++
		case "attachment":
			if phase != 2 || assistants != 0 {
				return ErrProtocol
			}
			attachmentKind, err := validateExchangeAttachment(m["attachment"], s.id)
			if err != nil || attachments[attachmentKind] {
				return ErrProtocol
			}
			attachments[attachmentKind] = true
			h, err := archiveContentHash(m["attachment"])
			if err != nil {
				return err
			}
			nativeAttachments = append(nativeAttachments, NativeArchiveAttachment{Type: attachmentKind, ContentHash: h, JSONBytes: len(m["attachment"])})
		case "assistant":
			if raw, ok := m["isMeta"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
				return ErrProtocol
			}
			if phase != 2 || assistants >= len(q.archiveMessages) || id != q.archiveMessages[assistants].id {
				return ErrProtocol
			}
			message, err := object(m["message"])
			if err != nil {
				return err
			}
			messageID, err := stringField(message, "id")
			if err != nil || messageID != q.messageID {
				return ErrProtocol
			}
			if _, err := firstAssistantText(m["message"], q.summary.Model); err != nil {
				return err
			}
			hash, err := archiveContentHash(message["content"])
			if err != nil || hash != q.archiveMessages[assistants].contentHash {
				return ErrProtocol
			}
			assistants++
		}
		parent = id
		return nil
	})
	if err != nil {
		return archiveVerification{}, err
	}
	if phase != 2 || assistants == 0 || assistants != len(q.archiveMessages) {
		return archiveVerification{}, ErrProtocol
	}
	reference.Persisted = true
	verified.receipt = reference
	verified.exchange = &ExchangeArchiveReceipt{Reference: reference, QuestionID: q.id, QuestionHash: q.hash, AnswerHash: hashText(q.answer), AssistantRecords: assistants, NativeAttachments: nativeAttachments, Persisted: true}
	return verified, nil
}

// JSON field order/whitespace is not content. Numbers are kept without float64
// rounding, and the complete content blocks (including signatures) are bound.
func archiveContentHash(raw json.RawMessage) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return "", ErrProtocol
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", ErrProtocol
	}
	return hashText(string(canonical)), nil
}
