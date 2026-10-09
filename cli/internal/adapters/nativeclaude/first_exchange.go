package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"
)

var (
	ErrAdmission          = errors.New("native Claude first question was not admitted")
	ErrPermissionRequired = errors.New("native Claude first exchange requires an unsupported permission interaction")
	ErrUnsupported        = errors.New("native Claude interaction or transcript form is unsupported")
)

// Preserve the caller's typed failure without rendering private source details.
type admissionError struct{ cause error }

func (admissionError) Error() string          { return ErrAdmission.Error() }
func (e admissionError) String() string       { return e.Error() }
func (e admissionError) GoString() string     { return e.Error() }
func (e admissionError) Unwrap() error        { return e.cause }
func (e admissionError) Is(target error) bool { return target == ErrAdmission }

// FirstExchange is an owned first-question transport for 2.1.287. The CLI
// composition supplies source/budget admission and user interaction handlers.
// The transport does not itself grant tools or measure exact tokens.
// Verified readback can issue a same-session resume plan. Configuration and
// native permission settings are never changed here. Bounded tool rounds and
// per-invocation interactions are supported; compaction and subagents are not.
// Existing native rules may have executed a preapproved tool before its frame
// arrives; rejecting that frame is not a tool sandbox or a zero-side-effect proof.
// Native question preprocessing stays enabled. This API does not claim normal
// TUI prompt/context equivalence.
type FirstExchange struct {
	s *Session
}

// FirstQuestionEvidence belongs to the very process which will receive the
// question. Admission must check the caller's prepared package, question budget
// and freshly authorized source; a native estimate alone is insufficient.
type FirstQuestionEvidence struct {
	Summary       ContextSummary
	Reference     ReferenceReceipt
	QuestionHash  string
	QuestionBytes int
}

// FirstExchangeResult is returned only after correlated completion and a clean
// EOF/cleanup audit. Completed is a native response, not exact archive readback,
// power-loss durability, provider capacity or acceptance of a full 800k input.
type FirstExchangeResult struct {
	SessionID    string `json:"session_id"`
	MessageID    string `json:"message_id"`
	QuestionHash string `json:"question_hash"`
	Answer       string `json:"answer"`
	Completed    bool   `json:"completed"`
}

func (FirstExchangeResult) String() string     { return "native Claude response (private content)" }
func (r FirstExchangeResult) GoString() string { return r.String() }
func (FirstExchange) String() string           { return "native Claude first exchange (private invocation)" }
func (e FirstExchange) GoString() string       { return e.String() }

func StartFirstExchange(ctx context.Context, opts Options) (*FirstExchange, error) {
	s, err := startVersion(ctx, opts, supportedVersion)
	if err != nil {
		return nil, err
	}
	return &FirstExchange{s: s}, nil
}

func (e *FirstExchange) SessionID() string   { return e.s.SessionID() }
func (e *FirstExchange) HostVersion() string { return e.s.HostVersion() }
func (e *FirstExchange) ContextSummary(ctx context.Context) (ContextSummary, error) {
	return e.s.ContextSummary(ctx)
}
func (e *FirstExchange) AppendReference(ctx context.Context, text string) (ReferenceReceipt, error) {
	return e.s.AppendReference(ctx, text)
}
func (e *FirstExchange) Close() error { return e.s.Close() }

// RunOrdinary preserves native question preprocessing and native tool policy.
// It owns one root question, possibly several model/tool rounds. Handlers must
// honor cancellation and must not reenter this transport. No callback can
// install permission rules. Slash commands and specialized dialogs/subagents
// remain unsupported; this is not a TUI-equivalence or exact-token claim.
// One attempt is consumed even by rejected admission or an uncertain result.
// The caller supplies a finite deadline. Only the winning concurrent call owns
// closing the session; no question is automatically retried.
func (e *FirstExchange) RunOrdinary(ctx context.Context, question string, admit func(context.Context, FirstQuestionEvidence) error, handlers InteractionHandlers) (result FirstExchangeResult, err error) {
	if e == nil || e.s == nil {
		return result, ErrState
	}
	s := e.s
	s.mu.Lock()
	if s.firstAttempted {
		s.mu.Unlock()
		return result, ErrState
	}
	s.firstAttempted = true
	s.mu.Unlock()
	// Admission is caller code. Even if it returns slowly after cancellation,
	// it cannot keep the native process alive or release a late question.
	stopCancel := context.AfterFunc(ctx, func() {
		s.fail(ctx.Err())
		_ = s.Close()
	})
	defer stopCancel()
	defer func() {
		if err != nil {
			s.fail(err)
		}
		closeErr := s.Close()
		err = errors.Join(err, closeErr, ctx.Err())
		if err != nil {
			result = FirstExchangeResult{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, bounded := ctx.Deadline(); !bounded || question == "" || !utf8.ValidString(question) {
		return result, ErrState
	}
	if len(question) > MaxReferenceBytes {
		return result, ErrLimit
	}
	if strings.HasPrefix(strings.TrimSpace(question), "/") {
		return result, ErrUnsupported
	}
	if admit == nil {
		return result, ErrAdmission
	}
	if err := s.enter(ctx); err != nil {
		return result, err
	}
	defer func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		<-s.gate
	}()
	s.mu.Lock()
	reference := s.receipt
	ready := s.appendPhase == 3 && reference.NoTurnAcknowledged && s.firstQuestion == nil
	s.mu.Unlock()
	if !ready {
		return result, ErrState
	}
	if err := s.launch.validate(); err != nil {
		return result, err
	}
	before, err := s.firstQuestionSummary(ctx)
	if err != nil {
		return result, err
	}
	// Do not share the optional threshold pointer with the admission callback.
	evidence := FirstQuestionEvidence{Summary: cloneContextSummary(before), Reference: reference, QuestionHash: hashText(question), QuestionBytes: len(question)}
	if err := admit(ctx, evidence); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, admissionError{cause: err}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	after, err := s.firstQuestionSummary(ctx)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(before, after) {
		return result, ErrAdmission
	}
	if err := s.launch.validate(); err != nil {
		return result, err
	}
	id, err := newUUID()
	if err != nil {
		return result, ErrState
	}
	q := &firstQuestionState{id: id, hash: hashText(question), bytes: len(question), summary: after}
	frame := map[string]any{"type": "user", "uuid": id, "session_id": s.id, "parent_tool_use_id": nil,
		"shouldQuery": true,
		"message":     map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": question}}}}
	raw, err := json.Marshal(frame)
	if err != nil {
		return result, ErrState
	}
	// Close owns cancellation after auditing committed permission writes.
	// Native can finish before the last writer returns from its syscall.
	q.ordinary = newOrdinaryState(ctx, handlers)
	s.mu.Lock()
	s.firstQuestion = q
	s.mu.Unlock()
	_, err = s.exchange(ctx, &pendingCall{id: id, kind: "first_question", result: make(chan json.RawMessage, 1)}, raw)
	if err != nil {
		return result, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || !q.completed {
		return result, ErrProtocol
	}
	return FirstExchangeResult{SessionID: s.id, MessageID: id, QuestionHash: q.hash, Answer: q.answer, Completed: true}, nil
}

func (s *Session) firstQuestionSummary(ctx context.Context) (ContextSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	raw, err := s.call(ctx, "get_context_usage", map[string]any{"subtype": "get_context_usage", "detail": "summary"})
	if err != nil {
		return ContextSummary{}, err
	}
	return parseSummary(raw, s.id)
}

func cloneContextSummary(s ContextSummary) ContextSummary {
	if s.AutoCompactThreshold != nil {
		v := *s.AutoCompactThreshold
		s.AutoCompactThreshold = &v
	}
	return s
}
