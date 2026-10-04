package nativecodex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Inspection is the default. The optional generation state permits only turns
// and correlated approvals on the owned thread; config writes and forks remain
// unavailable in either mode.
type handoffProtocol struct {
	mu                                                  sync.Mutex
	thread                                              Thread
	settings                                            map[string]json.RawMessage
	search                                              string
	pending                                             map[string]string
	initSent, initAck, initialized, resumeSent, resumed bool
	frames, bytes                                       int
	deniedAuxiliary                                     int
	generation                                          *generationProtocol
}

func newHandoffProtocol(thread Thread, raw json.RawMessage, search string) *handoffProtocol {
	settings, _ := rpcObject(raw)
	return &handoffProtocol{thread: thread, settings: settings, search: search, pending: map[string]string{}}
}

func handoffError(reason string) error { return fmt.Errorf("%w: handoff %s", ErrProtocol, reason) }

// Deny native title-generation requests locally. They must not create a second
// model request, consume the initial input budget, or disconnect the owned turn.
type deniedAuxiliaryRequest struct{ id json.RawMessage }

func (*deniedAuxiliaryRequest) Error() string { return "auxiliary generation unavailable" }

func (p *handoffProtocol) observe(data []byte, client bool) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(data) > maxMessageBytes {
		return false, handoffError("frame limit")
	}
	if !p.resumed {
		p.frames++
		p.bytes += len(data)
		if p.frames > 512 || p.bytes > maxNotificationBytes {
			return false, handoffError("initialization limit")
		}
	}
	f, ok := rpcObject(data)
	if !ok {
		return false, handoffError("invalid object")
	}
	for k := range f {
		switch k {
		case "jsonrpc", "id", "method", "params", "result", "error":
		case "emittedAtMs":
			if client || f["id"] != nil || f["method"] == nil {
				return false, handoffError("invalid notification timestamp")
			}
			if _, err := strconv.ParseUint(string(f[k]), 10, 64); err != nil {
				return false, handoffError("invalid notification timestamp")
			}
		default:
			return false, handoffError("unknown envelope field")
		}
	}
	if v, present := f["jsonrpc"]; present && string(v) != `"2.0"` {
		return false, handoffError("invalid version")
	}
	id, hasID := f["id"]
	key, validID := handoffID(id)
	if hasID && !validID {
		return false, handoffError("invalid request ID")
	}
	methodRaw, hasMethod := f["method"]
	_, hasResult := f["result"]
	_, hasError := f["error"]
	if hasMethod {
		var method string
		if hasResult || hasError || json.Unmarshal(methodRaw, &method) != nil || method == "" {
			return false, handoffError("invalid request")
		}
		if !client {
			if hasID {
				if p.generation != nil {
					return false, p.generation.serverRequest(method, key, f["params"])
				}
				return false, handoffError("server request before generation enabled")
			}
			if p.generation != nil {
				return false, p.generation.notification(method, f["params"])
			}
			return false, nil // notifications never establish readiness
		}
		if !hasID {
			if method != "initialized" || !p.initAck || p.initialized {
				return false, handoffError("unexpected notification")
			}
			p.initialized = true
			return false, nil
		}
		if _, exists := p.pending[key]; exists || len(p.pending) >= 64 {
			return false, handoffError("duplicate or excessive requests")
		}
		params, ok := rpcObject(f["params"])
		if f["params"] == nil || string(f["params"]) == "null" {
			params, ok = map[string]json.RawMessage{}, true
		}
		if !ok {
			return false, handoffError("invalid parameters")
		}
		if method == "initialize" {
			if p.initSent {
				return false, handoffError("repeated initialization")
			}
			if p.generation != nil && !generationNotificationsEnabled(params) {
				return false, handoffError("generation requires unsuppressed notifications")
			}
			p.initSent = true
		} else {
			if !p.initialized {
				return false, handoffError("not initialized")
			}
			switch method {
			case "thread/start":
				if p.resumed && p.generation != nil && p.deniedAuxiliary < 16 &&
					string(params["ephemeral"]) == "true" && stringEquals(params["threadSource"], "thread_title") {
					p.deniedAuxiliary++
					return false, &deniedAuxiliaryRequest{id: append(json.RawMessage(nil), id...)}
				}
				return false, handoffError("additional thread not permitted")
			case "turn/start", "turn/interrupt":
				if !p.resumed || p.generation == nil {
					return false, handoffError("generation not enabled")
				}
				if err := p.generation.clientRequest(p, method, key, params); err != nil {
					return false, err
				}
			case "thread/resume":
				if p.resumeSent || !p.resumeParams(params) {
					return false, handoffError("resume changes prepared intent")
				}
				p.resumeSent = true
			case "thread/read", "thread/turns/list", "thread/items/list", "thread/goal/get":
				if !stringEquals(params["threadId"], p.thread.ID) {
					return false, handoffError("different thread requested")
				}
			case "account/read", "config/read", "configRequirements/read", "hooks/list", "model/list", "collaborationMode/list", "thread/loaded/list", "thread/list", "skills/list", "plugin/list", "app/list", "mcpServerStatus/list":
			default:
				return false, handoffError("operation requires a separate generation or mutation gate")
			}
		}
		p.pending[key] = method
		return false, nil
	}
	if !hasID || hasResult == hasError || f["params"] != nil {
		return false, handoffError("unexpected response")
	}
	if client {
		if p.generation == nil {
			return false, handoffError("unexpected client response")
		}
		return false, p.generation.clientResponse(key, f)
	}
	method, exists := p.pending[key]
	if !exists {
		return false, handoffError("uncorrelated response")
	}
	delete(p.pending, key)
	if method == "turn/start" {
		return false, p.generation.startResponse(key, f["result"], hasError)
	}
	if hasError {
		if method == "initialize" || method == "thread/resume" {
			return false, handoffError("native initialization or resume rejected")
		}
		return false, nil // inspection errors can be displayed by the native UI
	}
	switch method {
	case "initialize":
		init, ok := identityObject(f["result"], "userAgent")
		var host string
		if !ok || json.Unmarshal(init["userAgent"], &host) != nil || host == "" || len(host) > 512 || strings.IndexFunc(host, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return false, handoffError("invalid initialization acknowledgement")
		}
		p.initAck = true
	case "thread/resume":
		if !p.resumeResult(f["result"]) {
			return false, handoffError("resume identity or settings mismatch")
		}
		p.resumed = true
		return true, nil
	}
	return false, nil
}

func handoffID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || len(raw) > 256 {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" || len(s) > 128 || strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return "", false
		}
		return "s:" + s, true
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != string(raw) {
		return "", false
	}
	return "n:" + string(raw), true
}

func stringEquals(raw json.RawMessage, want string) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == want
}

// Codex 0.157.1 uses Option<Option<String>> for serviceTier: omission inherits,
// but explicit null selects "default" (core/src/config/mod.rs and
// core/src/session/step_settings.rs at rust-v0.157.1). Check only present fields;
// an unspecified acknowledged tier is not proof of an explicit default choice.
func preservesServiceTier(value, prepared json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return stringEquals(prepared, "default")
	}
	var tier string
	return json.Unmarshal(value, &tier) == nil && tier != "" && stringEquals(prepared, tier)
}

func (p *handoffProtocol) resumeParams(params map[string]json.RawMessage) bool {
	if !stringEquals(params["threadId"], p.thread.ID) {
		return false
	}
	for key, value := range params {
		// Most null optional fields inherit; serviceTier explicitly clears.
		// Unknown fields cannot silently change the prepared thread contract.
		switch key {
		case "threadId":
		case "model", "modelProvider", "cwd", "approvalPolicy", "approvalsReviewer":
			if string(value) != "null" && !sameJSON(value, p.settings[key]) {
				return false
			}
		case "serviceTier":
			if !preservesServiceTier(value, p.settings[key]) {
				return false
			}
		case "sandbox":
			if string(value) != "null" {
				var mode string
				sandbox, ok := rpcObject(p.settings["sandbox"])
				if !ok || json.Unmarshal(value, &mode) != nil || !stringEquals(sandbox["type"], map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[mode]) {
					return false
				}
			}
		case "config":
			if string(value) != "null" {
				config, ok := rpcObject(value)
				if !ok {
					return false
				}
				for k, v := range config {
					if k != "web_search" || !stringEquals(v, p.search) {
						return false
					}
				}
			}
		case "history", "path", "baseInstructions", "developerInstructions", "personality", "permissions", "runtimeWorkspaceRoots", "initialTurnsPage":
			if string(value) != "null" {
				return false
			}
		case "excludeTurns":
			if string(value) != "true" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func sameJSON(a, b json.RawMessage) bool {
	canonical := func(raw []byte) []byte {
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&v) != nil {
			return nil
		}
		out, _ := json.Marshal(v)
		return out
	}
	aa, bb := canonical(a), canonical(b)
	return aa != nil && bb != nil && bytes.Equal(aa, bb)
}

func (p *handoffProtocol) resumeResult(raw json.RawMessage) bool {
	thread, ok := freshThread(raw, p.thread.Cwd, p.thread.ModelProvider)
	if !ok || thread.ID != p.thread.ID || thread.Model != p.thread.Model {
		return false
	}
	f, ok := rpcObject(raw)
	if !ok {
		return false
	}
	// These fields only paginate/restate the resumed history. Every other
	// native setting (including future fields) must match thread/start exactly.
	for _, key := range []string{"collaborationMode", "initialTurnsPage", "turnsBackwardsCursor", "itemsBackwardsCursor"} {
		for name := range f {
			if name != key && strings.EqualFold(name, key) {
				return false
			}
		}
		if key == "collaborationMode" && f[key] != nil && string(f[key]) != "null" {
			mode, ok := identityObject(f[key], "mode", "settings")
			settings, valid := identityObject(mode["settings"], "model", "reasoning_effort", "developer_instructions")
			if !ok || !valid || len(mode) != 2 || len(settings) != 3 || !stringEquals(mode["mode"], "default") || !stringEquals(settings["model"], p.thread.Model) ||
				!sameJSON(settings["reasoning_effort"], p.settings["reasoningEffort"]) || string(settings["developer_instructions"]) != "null" {
				return false
			}
		} else if f[key] != nil && string(f[key]) != "null" {
			return false
		}
		delete(f, key)
	}
	encoded, _ := json.Marshal(f)
	hash, ok := threadSettings(encoded, thread, ThreadOptions{})
	return ok && hash == p.thread.SettingsHash
}
