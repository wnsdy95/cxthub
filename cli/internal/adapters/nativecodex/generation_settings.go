package nativecodex

import "encoding/json"

func (p *handoffProtocol) preservesDefaultInstructions(raw json.RawMessage) bool {
	return string(raw) == "null" || len(p.defaultInstructions) != 0 && sameJSON(raw, p.defaultInstructions)
}

// Stock 0.157.1 resolves null default-mode instructions on the first turn and
// returns them in thread/settings/updated. The TUI repeats that native value on
// later turns. Learn it only from the owned server after releasing an unchanged
// turn, never from client input, a preset name, or conversation text. Caller
// holds p.mu; the value stays private and never changes the preparation budget.
func (p *handoffProtocol) observeGenerationSettings(raw json.RawMessage) error {
	params, ok := identityObject(raw, "threadId", "threadSettings")
	if !ok || !stringEquals(params["threadId"], p.thread.ID) {
		return handoffError("generation settings thread mismatch")
	}
	if p.generation == nil || !p.resumed || !p.generation.active || !p.generation.released {
		return nil // an unsolicited update cannot grant client override authority
	}
	settings, ok := identityObject(params["threadSettings"], "model", "modelProvider", "cwd", "approvalPolicy", "approvalsReviewer", "sandboxPolicy", "serviceTier", "effort", "collaborationMode")
	if !ok {
		return handoffError("invalid generation settings")
	}
	for key, expected := range map[string]string{
		"model": "model", "modelProvider": "modelProvider", "cwd": "cwd",
		"approvalPolicy": "approvalPolicy", "approvalsReviewer": "approvalsReviewer",
		"sandboxPolicy": "sandbox", "serviceTier": "serviceTier", "effort": "reasoningEffort",
	} {
		if !sameJSON(settings[key], p.settings[expected]) {
			return handoffError("generation settings changed")
		}
	}
	mode, valid := identityObject(settings["collaborationMode"], "mode", "settings")
	values, validValues := identityObject(mode["settings"], "model", "reasoning_effort", "developer_instructions")
	if !valid || !validValues || len(mode) != 2 || len(values) != 3 || !stringEquals(mode["mode"], "default") || !stringEquals(values["model"], p.thread.Model) || !sameJSON(values["reasoning_effort"], p.settings["reasoningEffort"]) {
		return handoffError("generation collaboration settings changed")
	}
	instructions := values["developer_instructions"]
	if string(instructions) == "null" {
		return nil
	}
	if len(p.defaultInstructions) == 0 && p.generation.firstDone {
		return handoffError("default instructions were not established by the initial turn")
	}
	var text string
	if json.Unmarshal(instructions, &text) != nil || text == "" || len(p.defaultInstructions) != 0 && !sameJSON(instructions, p.defaultInstructions) {
		return handoffError("generation default instructions changed")
	}
	p.defaultInstructions = append(json.RawMessage(nil), instructions...)
	return nil
}
