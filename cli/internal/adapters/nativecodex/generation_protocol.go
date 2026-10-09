package nativecodex

import (
	"encoding/json"
	"unicode/utf8"
)

// generationProtocol is protected by handoffProtocol.mu. Client and server IDs
// have separate namespaces. Nothing grants an approval on the user's behalf.
type generationProtocol struct {
	thread                                 Thread
	firstKey, startKey, turnID             string
	firstPrompt                            string
	gateTaken, released, active, firstDone bool
	serverPending                          map[string]string
	observation                            *initialTurnObservation
	completed                              *GenerationObservation
}

func newGenerationProtocol(thread Thread) *generationProtocol {
	return &generationProtocol{thread: thread, serverPending: map[string]string{}}
}

// Usage cannot be attributed when a client hides reroutes, tools or compaction
// from the native notification stream. Never let caller negotiation selectively
// remove evidence before our collector sees it. Unknown capabilities need review.
func generationNotificationsEnabled(params map[string]json.RawMessage) bool {
	raw := params["capabilities"]
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	capabilities, ok := rpcObject(raw)
	if !ok {
		return false
	}
	for key, value := range capabilities {
		switch key {
		case "requestAttestation":
			if string(value) != "false" && string(value) != "null" {
				return false
			}
		case "experimentalApi":
			if string(value) != "true" && string(value) != "false" {
				return false
			}
		case "optOutNotificationMethods":
			var methods []string
			if string(value) != "null" && (json.Unmarshal(value, &methods) != nil || len(methods) != 0) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (g *generationProtocol) clientRequest(p *handoffProtocol, method, key string, params map[string]json.RawMessage) error {
	if !stringEquals(params["threadId"], g.thread.ID) {
		return handoffError("generation thread mismatch")
	}
	if method == "turn/interrupt" {
		if !g.active || g.turnID == "" || !stringEquals(params["turnId"], g.turnID) || len(params) != 2 {
			return handoffError("interrupt does not match active turn")
		}
		return nil
	}
	if g.active || g.startKey != "" || len(g.serverPending) > 0 {
		return handoffError("turn already in progress")
	}
	prompt, ok := p.generationParams(params)
	if !ok {
		return handoffError("turn changes prepared settings or input shape")
	}
	g.active = true
	g.turnID = ""
	g.startKey = key
	if g.firstKey == "" {
		g.firstKey = key
		g.firstPrompt = prompt
	}
	return nil
}

func (p *handoffProtocol) generationParams(params map[string]json.RawMessage) (string, bool) {
	var input []json.RawMessage
	if json.Unmarshal(params["input"], &input) != nil || len(input) != 1 {
		return "", false
	}
	item, ok := identityObject(input[0], "type", "text")
	var prompt string
	if !ok || !stringEquals(item["type"], "text") || json.Unmarshal(item["text"], &prompt) != nil || !utf8.ValidString(prompt) {
		return "", false
	}
	for k, v := range item {
		switch k {
		case "type", "text":
		case "text_elements":
			var elements []json.RawMessage
			if string(v) != "null" && (json.Unmarshal(v, &elements) != nil || len(elements) != 0) {
				return "", false
			}
		default:
			return "", false
		}
	}
	for k, v := range params {
		switch k {
		case "threadId", "input":
		case "model", "cwd", "approvalPolicy", "approvalsReviewer":
			if string(v) != "null" && !sameJSON(v, p.settings[k]) {
				return "", false
			}
		case "serviceTier":
			if !preservesServiceTier(v, p.settings[k]) {
				return "", false
			}
		case "sandboxPolicy":
			if string(v) != "null" && !sameJSON(v, p.settings["sandbox"]) {
				return "", false
			}
		case "effort":
			if string(v) != "null" && !sameJSON(v, p.settings["reasoningEffort"]) {
				return "", false
			}
		case "clientUserMessageId":
			var id string
			if string(v) != "null" && (json.Unmarshal(v, &id) != nil || !validID(id)) {
				return "", false
			}
		case "collaborationMode":
			if string(v) != "null" {
				mode, ok := identityObject(v, "mode", "settings")
				settings, valid := identityObject(mode["settings"], "model", "reasoning_effort", "developer_instructions")
				if !ok || !valid || len(mode) != 2 || len(settings) != 3 || !stringEquals(mode["mode"], "default") || !stringEquals(settings["model"], p.thread.Model) || !sameJSON(settings["reasoning_effort"], p.settings["reasoningEffort"]) || !p.preservesDefaultInstructions(settings["developer_instructions"]) {
					return "", false
				}
			}
		case "runtimeWorkspaceRoots", "disabledPluginIds":
			if string(v) != "null" && !sameJSON(v, p.settings[k]) {
				return "", false
			}
		case "turnTrigger":
			if string(v) != "null" && !stringEquals(v, "user") {
				return "", false
			}
		case "summary", "outputSchema", "personality", "toolOutput", "serviceTierForTurn", "additionalContext", "cyberAccessProgram", "environments", "multiAgentMode", "permissions", "responsesapiClientMetadata":
			if string(v) != "null" {
				return "", false
			}
		default:
			return "", false
		}
	}
	return prompt, true
}

func (g *generationProtocol) bindTurn(raw json.RawMessage) error {
	f, ok := identityObject(raw, "id")
	var id string
	if !ok || json.Unmarshal(f["id"], &id) != nil || !validID(id) || (g.turnID != "" && g.turnID != id) {
		return handoffError("turn acknowledgement mismatch")
	}
	g.turnID = id
	if !g.firstDone && g.observation == nil {
		g.observation = newInitialTurnObservation(g.thread.ID, id)
	}
	return nil
}

func (g *generationProtocol) startResponse(key string, raw json.RawMessage, rejected bool) error {
	if key != g.startKey || !g.released {
		return handoffError("uncorrelated turn acknowledgement")
	}
	g.startKey = ""
	if rejected {
		// A JSON-RPC error does not prove that an upstream model never executed.
		if !g.firstDone {
			o := GenerationObservation{ThreadID: g.thread.ID, TurnID: g.turnID, Outcome: "unknown"}
			g.completed = &o
			g.firstDone = true
		}
		g.active = false
		clear(g.serverPending)
		return nil
	}
	f, ok := identityObject(raw, "turn")
	if !ok {
		return handoffError("invalid turn response")
	}
	return g.bindTurn(f["turn"])
}

func (g *generationProtocol) notification(method string, raw json.RawMessage) error {
	params, ok := rpcObject(raw)
	if !ok {
		return handoffError("invalid generation notification")
	}
	if id, exists := params["threadId"]; exists && !stringEquals(id, g.thread.ID) {
		return handoffError("foreign thread notification")
	}
	if method == "turn/started" {
		if !g.active || !g.released || !stringEquals(params["threadId"], g.thread.ID) {
			return handoffError("unrequested turn started")
		}
		if err := g.bindTurn(params["turn"]); err != nil {
			return err
		}
	}
	if !g.active {
		return nil
	}
	if id, exists := params["turnId"]; exists && !stringEquals(id, g.turnID) {
		return handoffError("foreign turn notification")
	}
	if g.observation != nil && !g.firstDone {
		if err := g.observation.Observe(method, raw); err != nil {
			return err
		}
	}
	if method == "turn/completed" {
		if !stringEquals(params["threadId"], g.thread.ID) || g.turnID == "" {
			return handoffError("completion without active turn")
		}
		if err := g.bindTurn(params["turn"]); err != nil {
			return err
		}
		if !g.firstDone {
			o := g.observation.Result()
			g.completed = &o
			g.firstDone = true
		}
		g.active = false
		clear(g.serverPending)
	}
	return nil
}

func (g *generationProtocol) serverRequest(method, key string, raw json.RawMessage) error {
	if !g.active || !g.released || g.turnID == "" || len(g.serverPending) >= 64 {
		return handoffError("server request outside active turn")
	}
	if _, exists := g.serverPending[key]; exists {
		return handoffError("duplicate server request")
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
	default:
		return handoffError("unsupported server request")
	}
	p, ok := identityObject(raw, "threadId", "turnId", "itemId")
	var item string
	if !ok || !stringEquals(p["threadId"], g.thread.ID) || !stringEquals(p["turnId"], g.turnID) || json.Unmarshal(p["itemId"], &item) != nil || !validID(item) {
		return handoffError("server request scope mismatch")
	}
	g.serverPending[key] = method
	return nil
}

func (g *generationProtocol) clientResponse(key string, fields map[string]json.RawMessage) error {
	if !g.active {
		return handoffError("reply outside active turn")
	}
	if _, ok := g.serverPending[key]; !ok {
		return handoffError("reply has no pending server request")
	}
	// The native server validates decision/answer shape. Relay the user's exact
	// correlated response (including a decline or error), never invent a choice.
	delete(g.serverPending, key)
	return nil
}
