package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// InteractionHandlers are invocation-scoped UI callbacks, not a permission
// policy. Native still applies its configured rules/hooks. Callbacks receive
// private copies and must honor cancellation; they never run on the reader or
// under Session.mu. An absent callback is not consent.
type InteractionHandlers struct {
	CanUseTool      func(context.Context, ToolPermissionRequest) (ToolPermissionDecision, error)
	AskUserQuestion func(context.Context, UserQuestionRequest) (UserQuestionAnswers, error)
}

type ToolPermissionDecision uint8

const (
	AllowOnce ToolPermissionDecision = iota + 1
	DenyOnce
)

// Input is the exact native-proposed input, which may include native hook
// updates. The API deliberately offers no arbitrary edited-input or permanent
// permission-update surface. Display strings are untrusted native content.
type ToolPermissionRequest struct {
	SessionID, RequestID, ToolUseID, ToolName string
	Input                                     json.RawMessage
	Description, DecisionReason               string
	DefaultToNo                               bool
}

type UserQuestionOption struct{ Label, Description, Preview string }
type UserQuestion struct {
	Question, Header string
	Options          []UserQuestionOption
	MultiSelect      bool
}
type UserQuestionRequest struct {
	SessionID, RequestID, ToolUseID string
	Questions                       []UserQuestion
}

// Answers are keyed by exact displayed question text. Single-select questions
// take at most one answer; multiple selections remain arrays on the native
// wire. Custom answers are allowed. An empty non-nil map means intentionally
// unanswered; nil is an invalid callback result, never implicit approval.
type UserQuestionAnswers struct{ Answers map[string][]string }

func (ToolPermissionRequest) String() string     { return "native Claude permission (private content)" }
func (r ToolPermissionRequest) GoString() string { return r.String() }
func (UserQuestionRequest) String() string       { return "native Claude questions (private content)" }
func (r UserQuestionRequest) GoString() string   { return r.String() }
func (UserQuestionAnswers) String() string       { return "native Claude answers (private content)" }
func (r UserQuestionAnswers) GoString() string   { return r.String() }

var ErrInteraction = errors.New("native Claude interaction failed")

type interactionError struct{ cause error }

func (interactionError) Error() string          { return ErrInteraction.Error() }
func (e interactionError) String() string       { return e.Error() }
func (e interactionError) GoString() string     { return e.Error() }
func (e interactionError) Unwrap() error        { return e.cause }
func (e interactionError) Is(target error) bool { return target == ErrInteraction }

type ordinaryControl struct {
	id, toolID, hash           string
	responseHash               string
	cancel                     context.CancelFunc
	ctx                        context.Context
	settled, withdrawn, echoed bool
}

const maxPermissionInput = 256 << 10

// The pinned host echoes committed permission responses on stdout. This is
// an acknowledgement of our exact reply, not a new decision or root query.
// Session.mu is held by the wire reducer.
func (s *Session) ordinaryControlEcho(m map[string]json.RawMessage) error {
	q := s.firstQuestion
	if q.phase != 2 || q.resultReceived || q.completed || !exchangeKeys(m, "type", "response", "session_id") {
		return ErrProtocol
	}
	r, err := object(m["response"])
	if err != nil {
		return err
	}
	id, err := stringField(r, "request_id")
	c := q.ordinary.controls[id]
	if err != nil || c == nil || !c.settled || c.responseHash == "" || c.echoed {
		return ErrProtocol
	}
	hash, err := archiveContentHash(m["response"])
	if err != nil || hash != c.responseHash {
		return ErrProtocol
	}
	c.echoed = true
	return nil
}

// Session.mu is held. No UI callback or IO runs here.
func (s *Session) ordinaryControl(m map[string]json.RawMessage, kind string) error {
	q, o := s.firstQuestion, s.firstQuestion.ordinary
	if q.phase != 2 || q.resultReceived || q.completed || s.closing || o.ctx.Err() != nil {
		return ErrProtocol
	}
	id, err := stringField(m, "request_id")
	if err != nil {
		return err
	}
	if kind == "control_cancel_request" {
		c := o.controls[id]
		if c == nil || c.withdrawn {
			return ErrProtocol
		}
		// A withdrawal may race an already committed reply. Native ignores that
		// reply. Linearize future callback decisions at this state transition.
		c.withdrawn, c.settled = true, true
		c.cancel()
		return nil
	}
	r, err := object(m["request"])
	if err != nil {
		return err
	}
	subtype, err := stringField(r, "subtype")
	if err != nil {
		return err
	}
	if subtype != "can_use_tool" {
		return ErrPermissionRequired
	}
	hash, err := archiveContentHash(m["request"])
	if err != nil {
		return err
	}
	if c := o.controls[id]; c != nil {
		if c.hash == hash {
			// A replay already queued by native can race our committed reply.
			// Deduplicate without resending or reauthorizing that invocation.
			return nil
		}
		return ErrProtocol
	}
	live := 0
	for _, c := range o.controls {
		if !c.settled {
			live++
		}
	}
	if live >= 8 || len(o.controls) >= 64 {
		return ErrLimit
	}
	toolID, err := stringField(r, "tool_use_id")
	if err != nil {
		return err
	}
	name, err := stringField(r, "tool_name")
	if err != nil {
		return err
	}
	tool := o.tools[toolID]
	if tool == nil || tool.done || tool.name != name {
		return ErrProtocol
	}
	for _, c := range o.controls {
		if c.toolID == toolID && !c.settled {
			return ErrProtocol
		}
	}
	if _, exists := r["agent_id"]; exists {
		return ErrUnsupported
	}
	if len(r["input"]) > maxPermissionInput {
		return ErrLimit
	}
	input, err := object(r["input"])
	if err != nil {
		return err
	}
	req := ToolPermissionRequest{SessionID: s.id, RequestID: id, ToolUseID: toolID, ToolName: name, Input: append(json.RawMessage(nil), r["input"]...)}
	for key, dest := range map[string]*string{"description": &req.Description, "decision_reason": &req.DecisionReason} {
		if raw, ok := r[key]; ok {
			*dest, err = exchangeString(raw, 32<<10)
			if err != nil {
				return err
			}
		}
	}
	interactive := false
	for key, dest := range map[string]*bool{"default_to_no": &req.DefaultToNo, "requires_user_interaction": &interactive} {
		if raw, ok := r[key]; ok {
			*dest, err = exchangeBool(raw)
			if err != nil {
				return err
			}
		}
	}
	var questions UserQuestionRequest
	if name == "AskUserQuestion" {
		if o.handlers.AskUserQuestion == nil {
			return ErrPermissionRequired
		}
		questions, err = parseUserQuestions(input)
		if err != nil {
			return err
		}
		questions.SessionID, questions.RequestID, questions.ToolUseID = s.id, id, toolID
	} else if interactive || o.handlers.CanUseTool == nil {
		return ErrPermissionRequired
	}
	ctx, cancel := context.WithCancel(o.ctx)
	c := &ordinaryControl{id: id, toolID: toolID, hash: hash, ctx: ctx, cancel: cancel}
	o.controls[id] = c
	go s.answerOrdinaryControl(q, c, req, questions, input)
	return nil
}

func (s *Session) answerOrdinaryControl(q *firstQuestionState, c *ordinaryControl, req ToolPermissionRequest, questions UserQuestionRequest, input map[string]json.RawMessage) {
	o := q.ordinary
	var response map[string]any
	var err error
	if req.ToolName == "AskUserQuestion" {
		// Preserve validation input independently of the caller-owned copy.
		shown, _ := parseUserQuestions(input)
		var answers UserQuestionAnswers
		answers, err = o.handlers.AskUserQuestion(c.ctx, questions)
		if err == nil {
			var updated map[string]json.RawMessage
			updated, err = questionAnswerInput(input, shown, answers)
			if err == nil {
				response = map[string]any{"behavior": "allow", "updatedInput": updated, "toolUseID": c.toolID, "decisionClassification": "user_temporary"}
			}
		}
	} else {
		var decision ToolPermissionDecision
		decision, err = o.handlers.CanUseTool(c.ctx, req)
		if err == nil {
			switch decision {
			case AllowOnce:
				response = map[string]any{"behavior": "allow", "toolUseID": c.toolID, "decisionClassification": "user_temporary"}
			case DenyOnce:
				response = map[string]any{"behavior": "deny", "message": "Permission declined", "toolUseID": c.toolID, "decisionClassification": "user_reject"}
			default:
				err = ErrInteraction
			}
		}
	}
	var reply, raw []byte
	var responseHash string
	if err == nil {
		reply, err = json.Marshal(map[string]any{"subtype": "success", "request_id": c.id, "response": response})
		if err == nil {
			responseHash, err = archiveContentHash(reply)
		}
		if err == nil {
			raw, err = json.Marshal(map[string]any{"type": "control_response", "response": json.RawMessage(reply)})
		}
	}
	// Serialize responses with the root query. Validate again at commit; no
	// callback can grant after a received withdrawal, shutdown or cancellation.
	s.writeMu.Lock()
	s.mu.Lock()
	active := s.firstQuestion == q && !s.closing && s.err == nil && !q.completed && !q.resultReceived && !c.settled && c.ctx.Err() == nil
	if active && err == nil {
		c.settled = true
		c.responseHash = responseHash
	}
	s.mu.Unlock()
	if !active {
		s.writeMu.Unlock()
		return
	}
	if err != nil {
		s.fail(interactionError{cause: err})
		s.writeMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(o.ctx, operationTimeout)
	err = s.process.write(ctx, append(raw, '\n'))
	cancel()
	if err != nil {
		s.fail(err)
	}
	s.writeMu.Unlock()
}

func parseUserQuestions(input map[string]json.RawMessage) (UserQuestionRequest, error) {
	var result UserQuestionRequest
	// Extended text/number cards, annotations, and follow-up UI need their own
	// renderer. Do not turn them into a generic approval or modify shown fields.
	if !exchangeKeys(input, "questions", "metadata") {
		return result, ErrUnsupported
	}
	if raw, ok := input["metadata"]; ok {
		if _, err := object(raw); err != nil {
			return result, err
		}
	}
	var rows []json.RawMessage
	if json.Unmarshal(input["questions"], &rows) != nil || len(rows) == 0 || len(rows) > 4 {
		return result, ErrProtocol
	}
	seen := map[string]bool{}
	for _, raw := range rows {
		m, err := object(raw)
		if err != nil {
			return result, err
		}
		if !exchangeKeys(m, "question", "header", "options", "multiSelect", "kind") {
			return result, ErrUnsupported
		}
		if raw, ok := m["kind"]; ok && !bytes.Equal(raw, []byte(`"choice"`)) {
			return result, ErrUnsupported
		}
		var q UserQuestion
		q.Question, err = exchangeString(m["question"], 8192)
		if err != nil || q.Question == "" || seen[q.Question] {
			return result, ErrProtocol
		}
		seen[q.Question] = true
		q.Header, err = exchangeString(m["header"], 256)
		if err != nil {
			return result, err
		}
		if raw, ok := m["multiSelect"]; ok {
			q.MultiSelect, err = exchangeBool(raw)
			if err != nil {
				return result, err
			}
		}
		var options []json.RawMessage
		if json.Unmarshal(m["options"], &options) != nil || len(options) < 2 || len(options) > 4 {
			return result, ErrProtocol
		}
		labels := map[string]bool{}
		for _, raw := range options {
			m, err := object(raw)
			if err != nil || !exchangeKeys(m, "label", "description", "preview") {
				return result, ErrUnsupported
			}
			var opt UserQuestionOption
			opt.Label, err = exchangeString(m["label"], 8192)
			if err != nil || opt.Label == "" || labels[opt.Label] {
				return result, ErrProtocol
			}
			labels[opt.Label] = true
			for k, dest := range map[string]*string{"description": &opt.Description, "preview": &opt.Preview} {
				if raw, ok := m[k]; ok {
					*dest, err = exchangeString(raw, 8192)
					if err != nil {
						return result, err
					}
				}
			}
			q.Options = append(q.Options, opt)
		}
		result.Questions = append(result.Questions, q)
	}
	return result, nil
}

func questionAnswerInput(input map[string]json.RawMessage, shown UserQuestionRequest, answers UserQuestionAnswers) (map[string]json.RawMessage, error) {
	if answers.Answers == nil {
		return nil, ErrInteraction
	}
	questions := map[string]UserQuestion{}
	for _, q := range shown.Questions {
		questions[q.Question] = q
	}
	values := map[string]any{}
	total := 0
	for question, selections := range answers.Answers {
		q, ok := questions[question]
		if !ok || !q.MultiSelect && len(selections) > 1 || len(selections) > len(q.Options)+1 {
			return nil, ErrInteraction
		}
		for _, answer := range selections {
			if !utf8.ValidString(answer) {
				return nil, ErrInteraction
			}
			n := len(utf16.Encode([]rune(answer)))
			if n > 8192 {
				return nil, ErrInteraction
			}
			total += n
		}
		// Native normalizes multi-select arrays and adds separators. Reserve
		// those units rather than passing a callback-created oversized result.
		if len(selections) > 1 {
			total += 2 * (len(selections) - 1)
		}
		if total > 32768 {
			return nil, ErrInteraction
		}
		if q.MultiSelect {
			values[question] = append([]string{}, selections...)
		} else if len(selections) == 1 {
			values[question] = selections[0]
		} else {
			values[question] = ""
		}
	}
	updated := make(map[string]json.RawMessage, len(input)+1)
	for k, v := range input {
		updated[k] = append(json.RawMessage(nil), v...)
	}
	updated["answers"], _ = json.Marshal(values)
	return updated, nil
}
