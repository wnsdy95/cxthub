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

// PrepareIdleResume binds a completed-exchange archive to the frozen invocation
// and a one-shot launch. No question is sent again or archive rewritten.
func (e *FirstExchange) PrepareIdleResume(ctx context.Context, path string) (*IdleResumePlan, error) {
	if e == nil || e.s == nil {
		return nil, ErrState
	}
	return e.s.prepareIdleResume(ctx, path)
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
	valid := s.version == supportedVersion && s.closeErr == nil && s.err == nil && q != nil && q.ordinary != nil && q.completed && reference.NoTurnAcknowledged
	s.mu.Unlock()
	if !valid {
		return archiveVerification{}, ErrState
	}
	// Protocol EOF and Close have completed: the exchange is now immutable.
	return s.verifyOrdinaryArchive(ctx, path, q, reference)
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
