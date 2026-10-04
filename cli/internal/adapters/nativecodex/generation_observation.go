package nativecodex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// GenerationObservation contains only metadata for the submitted initial turn.
// ModelContextWindow is native telemetry, never proof of provider capacity.
// Ineligible means the parent must not submit this observation for calibration.
type GenerationObservation struct {
	ThreadID, TurnID      string
	Outcome               string // completed, input_rejected, initial_compaction, unknown
	ExecutionKnown        bool
	ExecutionStarted      bool
	UsageKnown            bool
	UsageBeforeCompaction bool
	TotalInputTokens      int
	ModelContextWindow    int
	ModelRerouted         bool
	Ineligible            bool
}

const maxInitialObservationEvents = 4096

// initialTurnObservation consumes the ordered, uninterrupted v2 notification
// stream for the initial turn identified by turn/started or a turn/start ACK.
// The parent must replay notifications buffered while obtaining that identity;
// an ACK itself proves nothing about execution.
// It retains no event, item, output, prompt, model name, or error body.
//
// Pinned contract: Codex 0.157.1, rust-v0.157.1 / 36650394c5b38c2990ccf2a3457165ca3e9d9726.
// ThreadTokenUsageUpdatedNotification has no model-request identity. Moreover,
// core/src/session/turn.rs emits TokenCount after draining tools, and can emit
// stale/synthetic usage on errors. Therefore only the first usage notification
// following current-turn model output, without tools, errors or compaction, is
// accepted. Tool-bearing first requests can remain unknown. Later samples never
// replace that first sample. A dropped/reordered stream cannot establish usage.
// The v2 error schema has no no-execution witness: even contextWindowExceeded
// cannot set ExecutionKnown=true, ExecutionStarted=false or authorize a retry.
type initialTurnObservation struct {
	mu                sync.Mutex
	result            GenerationObservation
	events, bytes     int
	started, terminal bool
	firstClosed       bool
	modelOutput       bool
	compacted         bool
	compactionStarted bool
	failed            bool
	laterActivity     bool
}

func newInitialTurnObservation(threadID, turnID string) *initialTurnObservation {
	o := &initialTurnObservation{result: GenerationObservation{
		ThreadID: threadID, TurnID: turnID, Outcome: "unknown",
	}}
	if !validID(threadID) || !validID(turnID) {
		o.result.Ineligible = true
		o.failed = true
	}
	return o
}

func (o *initialTurnObservation) Result() GenerationObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.result
}

// Observe never performs IO. Protocol errors are sticky, bounded and sanitized;
// the parent must stop collecting on error. Unrelated thread/turn events are
// ignored after exact identity checks, and still count against the budget.
func (o *initialTurnObservation) Observe(method string, params json.RawMessage) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed {
		return o.fail("unavailable observation")
	}
	if method == "" || len(method) > 256 || len(params) > maxMessageBytes ||
		o.events >= maxInitialObservationEvents || len(params)+len(method) > maxNotificationBytes-o.bytes {
		return o.fail("event budget or size exceeded")
	}
	o.events++
	o.bytes += len(params) + len(method)
	switch method {
	case "turn/started", "turn/completed", "thread/tokenUsage/updated", "thread/compacted", "model/rerouted", "error":
	default:
		if !strings.HasPrefix(method, "item/") {
			return nil // includes turn/start ACKs, thread totals and legacy events
		}
	}
	p, ok := identityObject(params, "threadId")
	if !ok || !observationID(p["threadId"]) {
		return o.fail("invalid thread identity")
	}
	if !stringEquals(p["threadId"], o.result.ThreadID) {
		return nil
	}
	if !observationFieldNames(p, "turn", "turnId", "tokenUsage", "item", "itemId", "delta", "willRetry", "error") {
		return o.fail("ambiguous notification fields")
	}
	var turn map[string]json.RawMessage
	if method == "turn/started" || method == "turn/completed" {
		turn, ok = identityObject(p["turn"], "id")
		if !ok || !observationID(turn["id"]) {
			return o.fail("invalid turn identity")
		}
		if !stringEquals(turn["id"], o.result.TurnID) {
			return nil
		}
		// Never silently prefer nested identity over a conflicting flat alias.
		if id, present := p["turnId"]; present && !stringEquals(id, o.result.TurnID) {
			return o.fail("conflicting turn identity")
		}
	} else {
		p, ok = identityObject(params, "threadId", "turnId")
		if !ok || !observationID(p["turnId"]) {
			return o.fail("invalid turn identity")
		}
		if !stringEquals(p["turnId"], o.result.TurnID) {
			return nil
		}
	}
	if method == "model/rerouted" {
		// The correlated notification alone invalidates the prepared model scope.
		// No model names or reroute reason need to be retained or trusted.
		o.result.ModelRerouted = true
		o.result.Ineligible = true
		o.result.Outcome = "unknown"
		o.clearUsage()
		return nil
	}
	if o.result.ModelRerouted || o.terminal {
		return nil
	}
	switch method {
	case "turn/started", "turn/completed":
		return o.observeTurn(method, turn)
	case "thread/tokenUsage/updated":
		return o.observeUsage(p["tokenUsage"])
	case "thread/compacted":
		o.observeCompaction(false)
	case "error":
		var willRetry bool
		if string(p["willRetry"]) != "true" && string(p["willRetry"]) != "false" {
			return o.fail("invalid retry flag")
		}
		_ = json.Unmarshal(p["willRetry"], &willRetry)
		return o.observeError(p["error"], willRetry)
	case "item/started", "item/completed":
		return o.observeItem(p["item"], method == "item/started")
	case "item/agentMessage/delta", "item/plan/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		var delta string
		if !observationID(p["itemId"]) || !observationString(p["delta"], &delta) {
			return o.fail("invalid output metadata")
		}
		if delta != "" {
			o.executed()
			o.modelOutput = true
		}
	default:
		// An unfamiliar item event can hide a tool or request boundary. Keep an
		// already measured first sample, but do not acquire a later one.
		o.firstClosed = true
	}
	return nil
}

func (o *initialTurnObservation) observeTurn(method string, turn map[string]json.RawMessage) error {
	var status string
	var items []json.RawMessage
	if !observationFieldNames(turn, "status", "items", "error") || !observationString(turn["status"], &status) || len(turn["items"]) == 0 || turn["items"][0] != '[' || json.Unmarshal(turn["items"], &items) != nil {
		return o.fail("invalid turn metadata")
	}
	if status != "failed" && len(turn["error"]) != 0 && string(turn["error"]) != "null" {
		return o.fail("error on nonfailed turn")
	}
	if method == "turn/started" {
		if status != "inProgress" {
			return o.fail("invalid started status")
		}
		if o.started || len(items) != 0 {
			o.firstClosed = true // repeated/replayed starts do not reset the collector
		}
		o.started = true
		return nil
	}
	if status != "completed" && status != "failed" && status != "interrupted" {
		return o.fail("invalid terminal status")
	}
	// Terminal items can establish positive execution evidence, but cannot
	// retroactively anchor a previously ambiguous usage notification.
	laterActivity := o.laterActivity
	for _, item := range items {
		if err := o.observeItem(item, false); err != nil {
			return err
		}
	}
	o.laterActivity = laterActivity // a snapshot is not a new request boundary
	if status == "failed" {
		if raw := turn["error"]; len(raw) != 0 && string(raw) != "null" {
			if err := o.observeError(raw, false); err != nil {
				return err
			}
		} else if o.result.Outcome != "initial_compaction" {
			o.result.Outcome = "unknown"
			if !o.laterActivity {
				o.clearUsage()
			}
		}
	} else if o.result.Outcome != "initial_compaction" {
		if status == "completed" {
			o.result.Outcome = "completed" // not by itself evidence of execution
		} else {
			o.result.Outcome = "unknown"
		}
	}
	o.terminal = true
	return nil
}

func (o *initialTurnObservation) observeItem(raw json.RawMessage, started bool) error {
	item, ok := identityObject(raw, "type", "id")
	var kind string
	if !ok || !observationID(item["id"]) || !observationString(item["type"], &kind) || kind == "" {
		return o.fail("invalid item metadata")
	}
	switch kind {
	case "userMessage", "hookPrompt": // input, never evidence of model execution
	case "contextCompaction":
		o.observeCompaction(started)
	case "agentMessage", "reasoning", "plan":
		o.executed()
		o.modelOutput = true
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "webSearch", "imageView", "imageGeneration", "sleep", "functionCallOutput":
		o.executed()
		o.firstClosed = true // v2 cannot identify the request owning the next usage
	default:
		o.firstClosed = true
	}
	return nil
}

func (o *initialTurnObservation) observeCompaction(started bool) {
	if !o.compacted && !o.result.ExecutionStarted && !o.firstClosed {
		o.result.Outcome = "initial_compaction"
	}
	// A completion-only compaction marker cannot establish where compaction
	// began relative to the saved usage. A seen start marker can.
	if !started && !o.compactionStarted {
		o.clearUsage()
	}
	if started && o.result.UsageKnown {
		o.laterActivity = true
	}
	o.compactionStarted = o.compactionStarted || started
	o.compacted = true
	o.firstClosed = true
}

func (o *initialTurnObservation) observeError(raw json.RawMessage, willRetry bool) error {
	e, ok := identityObject(raw, "message")
	var message string
	if !ok || !observationFieldNames(e, "codexErrorInfo") || !observationString(e["message"], &message) {
		return o.fail("invalid error metadata")
	}
	// The sampling error path can publish stale/synthetic last usage before
	// its error. Preserve a sample only when subsequent activity establishes
	// that this error belongs after the measured request.
	if !o.laterActivity {
		o.clearUsage()
	}
	// Do not accept invented executionStarted/ExecutionKnown flags: they are
	// not an attestation defined by the pinned native schema.
	if o.result.Outcome != "initial_compaction" {
		initial := !o.firstClosed || o.result.Outcome == "input_rejected"
		o.result.Outcome = "unknown"
		if initial && !o.result.ExecutionStarted && !willRetry &&
			stringEquals(e["codexErrorInfo"], "contextWindowExceeded") {
			o.result.Outcome = "input_rejected"
		}
	}
	o.firstClosed = true
	return nil
}

func (o *initialTurnObservation) observeUsage(raw json.RawMessage) error {
	u, ok := identityObject(raw, "last", "total")
	if !ok || !observationFieldNames(u, "modelContextWindow") {
		return o.fail("invalid token usage")
	}
	last, ok := observationUsageBreakdown(u["last"])
	if !ok {
		return o.fail("invalid last token counts")
	}
	if _, ok := observationUsageBreakdown(u["total"]); !ok {
		return o.fail("invalid total token counts")
	}
	window := 0
	if value, present := u["modelContextWindow"]; present && string(value) != "null" {
		var valid bool
		window, valid = observationCount(value)
		if !valid {
			return o.fail("invalid context window")
		}
	}
	if o.firstClosed {
		return nil
	}
	o.firstClosed = true // missing/zero/stale first usage cannot be replaced later
	if !o.modelOutput || o.compacted || last[2] == 0 {
		return nil
	}
	o.result.UsageKnown = true
	o.result.UsageBeforeCompaction = true
	o.result.TotalInputTokens = last[0] // inputTokens already includes cached input
	o.result.ModelContextWindow = window
	return nil
}

// Indexes: input, cached input, output, reasoning output, total, cache writes.
func observationUsageBreakdown(raw json.RawMessage) ([6]int, bool) {
	var counts [6]int
	fields := []string{"inputTokens", "cachedInputTokens", "outputTokens", "reasoningOutputTokens", "totalTokens"}
	u, ok := identityObject(raw, fields...)
	if !ok || !observationFieldNames(u, "cacheWriteInputTokens") {
		return counts, false
	}
	for i, field := range fields {
		if counts[i], ok = observationCount(u[field]); !ok {
			return counts, false
		}
	}
	if value, present := u["cacheWriteInputTokens"]; present {
		if counts[5], ok = observationCount(value); !ok {
			return counts, false
		}
	}
	return counts, counts[1] <= counts[0] && counts[5] <= counts[0] && counts[3] <= counts[2]
}

func observationCount(raw json.RawMessage) (int, bool) {
	n, err := strconv.ParseInt(string(raw), 10, strconv.IntSize)
	return int(n), err == nil && n >= 0
}

// Optional fields need the same case-alias rejection as identityObject's
// required fields; otherwise an alias can silently erase meaningful evidence.
func observationFieldNames(object map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		for key := range object {
			if key != name && strings.EqualFold(key, name) {
				return false
			}
		}
	}
	return true
}

func observationString(raw json.RawMessage, value *string) bool {
	return len(raw) != 0 && raw[0] == '"' && json.Unmarshal(raw, value) == nil
}

func observationID(raw json.RawMessage) bool {
	var id string
	return observationString(raw, &id) && validID(id)
}

func (o *initialTurnObservation) executed() {
	if o.result.UsageKnown {
		o.laterActivity = true
	}
	o.result.ExecutionKnown = true
	o.result.ExecutionStarted = true
	if o.result.Outcome == "input_rejected" {
		o.result.Outcome = "unknown"
	}
}

func (o *initialTurnObservation) clearUsage() {
	o.result.UsageKnown = false
	o.result.UsageBeforeCompaction = false
	o.result.TotalInputTokens = 0
	o.result.ModelContextWindow = 0
}

func (o *initialTurnObservation) fail(reason string) error {
	o.failed = true
	o.result.Ineligible = true
	o.result.Outcome = "unknown"
	o.firstClosed = true
	o.clearUsage()
	return fmt.Errorf("%w: initial turn observation %s", ErrProtocol, reason)
}
