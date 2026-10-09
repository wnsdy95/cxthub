package nativecodex

import (
	"encoding/json"
	"testing"
)

func defaultCollaboration(f handoffUnitFixture, instructions any) map[string]any {
	return map[string]any{"mode": "default", "settings": map[string]any{
		"model": f.p.thread.Model, "reasoning_effort": f.p.settings["reasoningEffort"], "developer_instructions": instructions,
	}}
}

func generationSettingsNotice(f handoffUnitFixture, mode map[string]any) map[string]any {
	s := map[string]any{"collaborationMode": mode}
	for key, original := range map[string]string{
		"model": "model", "modelProvider": "modelProvider", "cwd": "cwd", "approvalPolicy": "approvalPolicy",
		"approvalsReviewer": "approvalsReviewer", "sandboxPolicy": "sandbox", "serviceTier": "serviceTier", "effort": "reasoningEffort",
	} {
		s[key] = f.p.settings[original]
	}
	return map[string]any{"threadId": f.p.thread.ID, "threadSettings": s}
}

func TestGenerationFollowupRepeatsOnlyObservedNativeDefault(t *testing.T) {
	f := generationUnit(t)
	mode := defaultCollaboration(f, "synthetic native default instructions")
	notice := generationSettingsNotice(f, mode)
	// Even an owned notification before generation cannot authorize text.
	generationNotice(t, f, "thread/settings/updated", notice, false)
	if len(f.p.defaultInstructions) != 0 {
		t.Fatal("unsolicited settings granted instruction authority")
	}
	start := map[string]any{"threadId": f.p.thread.ID, "input": []any{map[string]any{"type": "text", "text": "next"}}, "collaborationMode": mode}
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "spoof", "turn/start", start), true, false, true)
	generationStart(t, f, "first")
	generationNotice(t, f, "thread/settings/updated", notice, false)
	generationNotice(t, f, "turn/started", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("inProgress")}, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "first", map[string]any{"turn": generationTurn("inProgress")}), false, false, false)
	generationNotice(t, f, "turn/completed", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("completed")}, false)
	// The second question preserves the observed native setting and first gate.
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "next", "turn/start", start), true, false, false)
	generationNotice(t, f, "thread/settings/updated", notice, false)
	if f.p.generation.firstKey != "s:first" || !f.p.generation.firstDone {
		t.Fatal("follow-up consumed a new initial gate")
	}
	if !f.p.preservesDefaultInstructions(json.RawMessage("null")) || f.p.preservesDefaultInstructions(json.RawMessage(`"arbitrary override"`)) {
		t.Fatal("default instruction identity was weakened")
	}
}

func TestGenerationCannotEstablishDefaultAfterFirstCompletion(t *testing.T) {
	f := generationUnit(t)
	generationStart(t, f, "first")
	generationNotice(t, f, "turn/started", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("inProgress")}, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "first", map[string]any{"turn": generationTurn("inProgress")}), false, false, false)
	generationNotice(t, f, "turn/completed", map[string]any{"threadId": f.p.thread.ID, "turn": generationTurn("completed")}, false)
	generationStart(t, f, "second")
	generationNotice(t, f, "thread/settings/updated", generationSettingsNotice(f, defaultCollaboration(f, "late native text")), true)
	if len(f.p.defaultInstructions) != 0 {
		t.Fatal("later turn established the initial instruction baseline")
	}
}

func TestGenerationDefaultBindingRejectsChangedIdentityAndSettings(t *testing.T) {
	for _, change := range []string{"thread", "model", "provider", "cwd", "permission", "sandbox", "effort", "mode", "mode-model", "mode-effort", "instructions-type", "duplicate", "alias", "changed-instructions"} {
		t.Run(change, func(t *testing.T) {
			f := generationUnit(t)
			generationStart(t, f, "first")
			mode := defaultCollaboration(f, "native default")
			notice := generationSettingsNotice(f, mode)
			s := notice["threadSettings"].(map[string]any)
			values := mode["settings"].(map[string]any)
			switch change {
			case "thread":
				notice["threadId"] = "foreign"
			case "model":
				s["model"] = "other"
			case "provider":
				s["modelProvider"] = "other"
			case "cwd":
				s["cwd"] = "/elsewhere"
			case "permission":
				s["approvalPolicy"] = "never"
			case "sandbox":
				s["sandboxPolicy"] = map[string]any{"type": "dangerFullAccess"}
			case "effort":
				s["effort"] = "high"
			case "mode":
				mode["mode"] = "plan"
			case "mode-model":
				values["model"] = "other"
			case "mode-effort":
				values["reasoning_effort"] = "high"
			case "instructions-type":
				values["developer_instructions"] = []any{"native default"}
			case "duplicate":
				s["collaborationMode"] = json.RawMessage(`{"mode":"default","mode":"plan","settings":{}}`)
			case "alias":
				mode["Mode"] = "default"
			case "changed-instructions":
				generationNotice(t, f, "thread/settings/updated", notice, false)
				values["developer_instructions"] = "different default"
			}
			generationNotice(t, f, "thread/settings/updated", notice, true)
			if change != "changed-instructions" && len(f.p.defaultInstructions) != 0 {
				t.Fatal("rejected update changed the instruction baseline")
			}
		})
	}
}
