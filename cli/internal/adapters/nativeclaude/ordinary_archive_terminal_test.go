//go:build darwin || linux

package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
)

// Extend the synthetic completed-tools recorder, never an installed Claude or
// a captured provider archive. Wire completion stays separate from metadata.
func ordinaryTerminalRows(t *testing.T, e *FirstExchange) []map[string]any {
	t.Helper()
	rows := ordinaryRecordedMetadata(t, e)
	var previous map[string]any
	for _, row := range rows {
		attachment, ok := row["attachment"].(map[string]any)
		if !ok {
			continue
		}
		switch attachment["type"] {
		case "prompt_snapshot":
			attachment["echoWireToolInputs"] = true
			previous = attachment
		case "session_context":
			row["renderedRole"] = "user"
			row["rendered"] = []any{map[string]any{"content": "Synthetic native session context"}}
		}
	}
	if previous == nil || rows[len(rows)-1]["type"] != "assistant" {
		t.Fatal("fixture lacks a prior snapshot or final assistant")
	}
	terminal := maps.Clone(previous)
	terminal["systemTurns"], terminal["toolChangeHeader"] = true, true
	terminal["inlineTools"], terminal["keptReminders"] = false, true
	rows = append(rows, map[string]any{
		"type": "attachment", "uuid": "terminal-snapshot", "parentUuid": rows[len(rows)-1]["uuid"],
		"sessionId": e.s.id, "cwd": e.s.cwd, "isSidechain": false, "entrypoint": "sdk-cli", "attachment": terminal,
	}, map[string]any{
		"type": "last-prompt", "sessionId": e.s.id, "lastPrompt": "ordinary question", "leafUuid": "terminal-snapshot",
	})
	return rows
}

func TestOrdinaryArchiveTerminalSnapshot(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	result, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{})
	if err != nil || !result.Completed {
		t.Fatal("synthetic tools exchange did not complete", err)
	}
	rows := ordinaryTerminalRows(t, e)
	path := unitArchive(e.s)
	writeExchangeRows(t, e, rows)
	proof, err := e.VerifyArchive(context.Background(), path)
	if err != nil || !proof.Persisted || proof.AssistantRecords != 2 || proof.ToolResultRecords != 1 || len(proof.NativeAttachments) != 9 {
		t.Fatal("terminal snapshot changed completed-tools proof", err)
	}
	terminal := rows[len(rows)-2]["attachment"]
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := archiveContentHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	last := proof.NativeAttachments[len(proof.NativeAttachments)-1]
	if last.Type != "prompt_snapshot" || last.ContentHash != hash || last.JSONBytes != len(raw) {
		t.Fatal("terminal metadata was omitted from the attachment receipt")
	}
	if _, err := e.PrepareIdleResume(context.Background(), path); err != nil {
		t.Fatal("terminal leaf prevented verified resume preparation", err)
	}
	baseline, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]map[string]any) []map[string]any
	}{
		{"changed-system-prompt", terminalSnapshotMutation("systemPrompt", []any{"Changed native instructions"})},
		{"changed-fold", terminalSnapshotMutation("reminderFold", true)},
		{"changed-echo", terminalSnapshotMutation("echoWireToolInputs", false)},
		{"changed-rendering", terminalSnapshotMutation("contextRendering", "other")},
		{"changed-existing-prefix", terminalSnapshotMutation("cliPrefix", "Replaced existing native prefix")},
		{"removed-existing-tools", terminalSnapshotMutation("tools", []any{})},
		{"changed-existing-tool", func(r []map[string]any) []map[string]any {
			tool := r[len(r)-2]["attachment"].(map[string]any)["tools"].([]any)[0].(map[string]any)
			tool["description"] = "Changed existing tool instructions"
			tool["schema"].(map[string]any)["description"] = tool["description"]
			return r
		}},
		{"removed-echo", func(r []map[string]any) []map[string]any {
			delete(r[len(r)-2]["attachment"].(map[string]any), "echoWireToolInputs")
			return r
		}},
		{"arbitrary-trailing-attachment", func(r []map[string]any) []map[string]any {
			r[len(r)-2]["attachment"] = map[string]any{"type": "total_tokens_reminder", "text": "Synthetic reminder"}
			return r
		}},
		{"duplicate-terminal", func(r []map[string]any) []map[string]any {
			i := len(r) - 2
			duplicate := maps.Clone(r[i])
			duplicate["uuid"], duplicate["parentUuid"] = "terminal-duplicate", r[i]["uuid"]
			r[len(r)-1]["leafUuid"] = duplicate["uuid"]
			return slices.Insert(r, i+1, duplicate)
		}},
		{"foreign-parent", func(r []map[string]any) []map[string]any {
			// An existing but wrong parent tests continuity, not just membership.
			r[len(r)-2]["parentUuid"] = r[1]["uuid"]
			return r
		}},
		{"last-leaf-is-final-assistant", func(r []map[string]any) []map[string]any {
			r[len(r)-1]["leafUuid"] = r[len(r)-3]["uuid"]
			return r
		}},
		{"foreign-last-leaf", func(r []map[string]any) []map[string]any {
			r[len(r)-1]["leafUuid"] = "foreign-leaf"
			return r
		}},
		{"stale-last-leaf-before-terminal", func(r []map[string]any) []map[string]any {
			i := len(r) - 2
			r[i+1]["leafUuid"] = r[i-1]["uuid"]
			r[i], r[i+1] = r[i+1], r[i]
			return r
		}},
		{"null-last-leaf", func(r []map[string]any) []map[string]any {
			r[len(r)-1]["leafUuid"] = nil
			return r
		}},
		{"rewound-last-leaf", func(r []map[string]any) []map[string]any {
			r[len(r)-1]["rewound"] = true
			return r
		}},
		{"missing-prior-snapshot", func(r []map[string]any) []map[string]any {
			// Repair each removed row's child edge so the missing snapshot is
			// the violated condition, rather than an incidental broken parent.
			for i := len(r) - 3; i >= 0; i-- {
				a, ok := r[i]["attachment"].(map[string]any)
				if !ok || a["type"] != "prompt_snapshot" {
					continue
				}
				for _, row := range r[i+1:] {
					if row["parentUuid"] == r[i]["uuid"] {
						row["parentUuid"] = r[i]["parentUuid"]
					}
				}
				r = slices.Delete(r, i, i+1)
			}
			return r
		}},
		{"rendered-terminal", func(r []map[string]any) []map[string]any {
			r[len(r)-2]["renderedRole"] = "system"
			r[len(r)-2]["rendered"] = []any{map[string]any{"content": "Synthetic rendered terminal instructions"}}
			return r
		}},
		{"session-context-as-system", func(r []map[string]any) []map[string]any {
			for _, row := range r {
				if a, ok := row["attachment"].(map[string]any); ok && a["type"] == "session_context" {
					row["renderedRole"] = "system"
				}
			}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed []map[string]any
			if err := json.Unmarshal(baseline, &changed); err != nil {
				t.Fatal(err)
			}
			writeExchangeRows(t, e, tc.mutate(changed))
			_, err := e.VerifyArchive(context.Background(), path)
			if !errors.Is(err, ErrProtocol) && !errors.Is(err, ErrUnsupported) {
				t.Fatal("invalid terminal contract was not rejected by semantic validation", err)
			}
		})
	}
	writeExchangeRows(t, e, rows)
	if _, err := e.VerifyArchive(context.Background(), path); err != nil {
		t.Fatal("baseline did not recover after rejected variants", err)
	}
}

func terminalSnapshotMutation(key string, value any) func([]map[string]any) []map[string]any {
	return func(rows []map[string]any) []map[string]any {
		rows[len(rows)-2]["attachment"].(map[string]any)[key] = value
		return rows
	}
}

func TestOrdinaryArchiveTerminalProgressiveCheckpoints(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	result, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{})
	if err != nil || !result.Completed {
		t.Fatal("synthetic tools exchange did not complete", err)
	}
	rows := ordinaryTerminalRows(t, e)
	if rows[0]["type"] != "user" || rows[1]["type"] != "user" || rows[len(rows)-3]["type"] != "assistant" {
		t.Fatal("fixture lacks the reference, question or final assistant checkpoint")
	}
	var progressive []map[string]any
	for i, row := range rows {
		progressive = append(progressive, row)
		if i == 0 || i == 1 || i == len(rows)-3 {
			progressive = append(progressive, map[string]any{
				"type": "last-prompt", "sessionId": e.s.id,
				"lastPrompt": "Synthetic checkpoint", "leafUuid": row["uuid"],
			})
		}
	}
	path := unitArchive(e.s)
	t.Run("final-selection-reaches-terminal-snapshot", func(t *testing.T) {
		writeExchangeRows(t, e, progressive)
		proof, err := e.VerifyArchive(context.Background(), path)
		if err != nil || !proof.Persisted || proof.AssistantRecords != 2 || proof.ToolResultRecords != 1 || len(proof.NativeAttachments) != 9 {
			t.Fatal("valid progressive checkpoints changed the completed exchange proof", err)
		}
	})
	t.Run("missing-final-selection", func(t *testing.T) {
		// Keep the terminal attachment, but leave the last explicit selection
		// at the previously valid final assistant. EOF must reject that gap.
		writeExchangeRows(t, e, progressive[:len(progressive)-1])
		if _, err := e.VerifyArchive(context.Background(), path); !errors.Is(err, ErrProtocol) {
			t.Fatal("stale final selection was not rejected", err)
		}
	})
}

func TestOrdinaryArchiveTerminalSnapshotBooleanTypes(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	rows := ordinaryTerminalRows(t, e)
	baseline, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reminderFold", "echoWireToolInputs", "keptReminders", "inlineTools", "systemTurns", "toolChangeHeader"} {
		for _, tc := range []struct {
			name  string
			value any
		}{{"null", nil}, {"string", "true"}, {"number", 1}, {"array", []any{}}, {"object", map[string]any{}}} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				var changed []map[string]any
				if err := json.Unmarshal(baseline, &changed); err != nil {
					t.Fatal(err)
				}
				writeExchangeRows(t, e, terminalSnapshotMutation(key, tc.value)(changed))
				if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
					t.Fatal("malformed snapshot boolean accepted")
				}
			})
		}
	}
}
