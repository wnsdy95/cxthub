//go:build darwin || linux

package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// Synthetic content with the measured 2.1.287 ordinary recorder structure.
// No provider prompt, real workspace metadata, or credentials are retained.
func ordinaryRecordedMetadata(t *testing.T, e *FirstExchange) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(unitArchive(e.s))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		row["entrypoint"] = "sdk-cli"
		records = append(records, row)
	}
	records[0]["origin"] = map[string]any{"kind": "unclassified"}
	records[0]["queueSkipAttachments"], records[0]["queueTranscriptOnly"] = true, true
	records[1]["promptSource"], records[1]["turnOrigin"] = "sdk", "sdk"
	records[1]["turnPosition"] = map[string]any{"promptIndex": 0, "turnIndex": 1}
	rows := append([]map[string]any{}, records[:2]...)
	parent := records[1]["uuid"]
	attach := func(id string, attachment map[string]any, rendered bool) {
		row := map[string]any{"type": "attachment", "uuid": id, "parentUuid": parent, "sessionId": e.s.id, "cwd": e.s.cwd, "isSidechain": false, "entrypoint": "sdk-cli", "attachment": attachment}
		if rendered {
			row["renderedRole"], row["rendered"] = "system", []any{map[string]any{"content": "Synthetic native reminder"}}
		}
		rows = append(rows, row)
		parent = id
	}
	prompt := func(tools bool) map[string]any {
		m := map[string]any{"type": "prompt_snapshot", "systemPrompt": []string{"Synthetic native instructions"}, "reminderFold": false, "echoWireToolInputs": false, "contextRendering": "announced"}
		if tools {
			m["systemTurns"], m["toolChangeHeader"], m["inlineTools"], m["keptReminders"], m["cliPrefix"] = true, true, false, false, "Synthetic prefix"
			m["tools"] = []any{map[string]any{"name": "Read", "description": "Synthetic tool", "schema": map[string]any{"name": "Read", "description": "Synthetic tool", "input_schema": map[string]any{"type": "object"}}}}
		}
		return m
	}
	attach("env", map[string]any{"type": "environment", "snapshot": map[string]any{"workingDirectory": e.s.cwd, "isGitRepo": true, "isWorktree": true, "additionalWorkingDirectories": []string{}, "platform": "darwin", "shell": "unknown", "osVersion": "synthetic"}}, true)
	attach("model", map[string]any{"type": "model", "identity": map[string]any{"modelId": "fixture-model", "marketingName": "Synthetic", "knowledgeCutoff": "synthetic"}, "text": "Synthetic model description"}, true)
	attach("reminder-1", map[string]any{"type": "total_tokens_reminder", "text": "Synthetic token reminder"}, true)
	attach("context", map[string]any{"type": "session_context", "context": map[string]any{}}, false)
	attach("date", map[string]any{"type": "date", "date": "2026-10-05"}, true)
	attach("prompt-1", prompt(false), false)
	for i, row := range records[2:] {
		if row["type"] == "assistant" {
			row["parentUuid"] = parent
			row["apiBlockIndex"], row["effort"], row["perTurnEffort"] = 0, "medium", "medium"
			row["thinkingDurationMs"], row["advisorModel"] = 775, "fixture-model"
		}
		rows = append(rows, row)
		parent = row["uuid"]
		if i == 1 {
			attach("prompt-2", prompt(true), false)
			attach("reminder-2", map[string]any{"type": "total_tokens_reminder", "text": "Another native reminder"}, true)
		}
	}
	return rows
}

func TestOrdinaryArchiveNativeMetadata(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	rows := ordinaryRecordedMetadata(t, e)
	writeExchangeRows(t, e, rows)
	proof, err := e.VerifyArchive(context.Background(), unitArchive(e.s))
	if err != nil || !proof.Persisted || len(proof.NativeAttachments) != 8 || proof.AssistantRecords != 2 || proof.ToolResultRecords != 1 {
		t.Fatalf("native metadata readback: %+v, %v", proof, err)
	}
	baseline, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]map[string]any){
		"reference-origin":         func(r []map[string]any) { r[0]["origin"].(map[string]any)["kind"] = "external" },
		"reference-queue":          func(r []map[string]any) { r[0]["queueSkipAttachments"] = false },
		"question-transcript-only": func(r []map[string]any) { r[1]["queueTranscriptOnly"] = true },
		"question-position":        func(r []map[string]any) { r[1]["turnPosition"].(map[string]any)["turnIndex"] = 2 },
		"foreign-ingress":          func(r []map[string]any) { r[1]["entrypoint"] = "external" },
		"foreign-environment": func(r []map[string]any) {
			r[2]["attachment"].(map[string]any)["snapshot"].(map[string]any)["workingDirectory"] = "/foreign"
		},
		"foreign-model": func(r []map[string]any) {
			r[3]["attachment"].(map[string]any)["identity"].(map[string]any)["modelId"] = "foreign"
		},
		"rendered-as-user":         func(r []map[string]any) { r[4]["renderedRole"] = "user" },
		"rendered-injection-field": func(r []map[string]any) { r[4]["rendered"].([]any)[0].(map[string]any)["ephemeral"] = true },
		"attachment-hides-message": func(r []map[string]any) { r[4]["message"] = r[1]["message"] },
		"folding-mode":             func(r []map[string]any) { r[7]["attachment"].(map[string]any)["reminderFold"] = true },
		"schema-changed-name": func(r []map[string]any) {
			r[10]["attachment"].(map[string]any)["tools"].([]any)[0].(map[string]any)["schema"].(map[string]any)["name"] = "Write"
		},
		"attachment-parent":             func(r []map[string]any) { r[10]["parentUuid"] = r[1]["uuid"] },
		"initial-announcement-repeated": func(r []map[string]any) { r[10]["attachment"] = r[3]["attachment"] },
		"assistant-block-index":         func(r []map[string]any) { r[8]["apiBlockIndex"] = 0.5 },
		"assistant-thinking-time":       func(r []map[string]any) { r[8]["thinkingDurationMs"] = -1 },
		"assistant-advisor-model":       func(r []map[string]any) { r[8]["advisorModel"] = "" },
		"question-advisor-model":        func(r []map[string]any) { r[1]["advisorModel"] = "fixture-model" },
		"assistant-hides-attachment":    func(r []map[string]any) { r[8]["attachment"] = r[3]["attachment"] },
	} {
		t.Run(name, func(t *testing.T) {
			var changed []map[string]any
			if err := json.Unmarshal(baseline, &changed); err != nil {
				t.Fatal(err)
			}
			mutate(changed)
			writeExchangeRows(t, e, changed)
			if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
				t.Fatal("behavior or lineage mutation accepted")
			}
		})
	}
	writeExchangeRows(t, e, rows)
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err != nil {
		t.Fatal("restored archive", err)
	}
	if _, err := e.PrepareIdleResume(context.Background(), unitArchive(e.s)); err != nil {
		t.Fatal("verified native resume", err)
	}
}

func TestOrdinaryArchiveKeepsNativeOrganizationMarker(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	e := f.start(t, true)
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	rows := ordinaryRecordedMetadata(t, e)
	marker := map[string]any{"type": "attachment", "uuid": "organization-marker", "parentUuid": rows[1]["uuid"], "sessionId": e.s.id, "cwd": e.s.cwd, "isSidechain": false,
		"attachment": map[string]any{"type": "credential_org", "organizationUuid": "11111111-1111-4111-8111-111111111111"}}
	rows[2]["parentUuid"] = marker["uuid"]
	rows = append(append(append([]map[string]any{}, rows[:2]...), marker), rows[2:]...)
	writeExchangeRows(t, e, rows)
	proof, err := e.VerifyArchive(context.Background(), unitArchive(e.s))
	if err != nil || !proof.Persisted || len(proof.NativeAttachments) != 9 {
		t.Fatal("organization marker was not retained", err)
	}
	baseline, _ := json.Marshal(rows)
	for _, bad := range []any{nil, "", "foreign-path", 42} {
		var changed []map[string]any
		_ = json.Unmarshal(baseline, &changed)
		changed[2]["attachment"].(map[string]any)["organizationUuid"] = bad
		writeExchangeRows(t, e, changed)
		if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err == nil {
			t.Fatal("malformed organization marker accepted")
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, []map[string]any) []map[string]any
	}{
		{"duplicate-initial-markers", func(_ *testing.T, r []map[string]any) []map[string]any {
			// Keep UUIDs unique and the parent chain intact so only duplicate
			// organization metadata, not malformed lineage, causes rejection.
			duplicate := maps.Clone(r[2])
			duplicate["uuid"], duplicate["parentUuid"] = "organization-marker-duplicate", r[2]["uuid"]
			r[3]["parentUuid"] = duplicate["uuid"]
			return slices.Insert(r, 3, duplicate)
		}},
		{"later-attachment-batch", func(t *testing.T, r []map[string]any) []map[string]any {
			// Move the sole marker to the otherwise valid post-tool attachment
			// batch, repairing both its old and new parent links.
			marker := r[2]
			r[3]["parentUuid"] = marker["parentUuid"]
			r = slices.Delete(r, 2, 3)
			i := slices.IndexFunc(r, func(row map[string]any) bool { return row["uuid"] == "prompt-2" })
			if i < 0 {
				t.Fatal("fixture has no later attachment batch")
			}
			marker["parentUuid"] = r[i]["parentUuid"]
			r[i]["parentUuid"] = marker["uuid"]
			return slices.Insert(r, i, marker)
		}},
		{"extra-payload-key", func(_ *testing.T, r []map[string]any) []map[string]any {
			r[2]["attachment"].(map[string]any)["permissions"] = []any{"synthetic-grant"}
			return r
		}},
		{"rendered-content", func(_ *testing.T, r []map[string]any) []map[string]any {
			// This rendering is valid for other native attachments, but an
			// organization marker must never contribute rendered instructions.
			r[2]["renderedRole"] = "system"
			r[2]["rendered"] = []any{map[string]any{"content": "Synthetic organization instructions"}}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed []map[string]any
			if err := json.Unmarshal(baseline, &changed); err != nil {
				t.Fatal(err)
			}
			changed = tc.mutate(t, changed)
			writeExchangeRows(t, e, changed)
			if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); !errors.Is(err, ErrUnsupported) {
				t.Fatal("unsupported organization marker was not rejected", err)
			}
		})
	}
	writeExchangeRows(t, e, rows)
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err != nil {
		t.Fatal("original marker changed", err)
	}
}

func TestOrdinaryArchivePreservesObservedSiblingOrder(t *testing.T) {
	f := ordinaryFixture(t, "sibling-tool-parent")
	e := f.start(t, true)
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "ordinary question", firstExchangeAllow, InteractionHandlers{CanUseTool: func(context.Context, ToolPermissionRequest) (ToolPermissionDecision, error) { return AllowOnce, nil }}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(unitArchive(e.s))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		first, second any
		accept        bool
	}{
		{"no_indices", nil, nil, false},
		{"stream_order", 0, 1, true},
		{"same_scalar_expansion", 0, 0, true},
		{"sparse_order_preserved", 2, 4, true},
		{"indices_reverse_content", 1, 0, false},
		{"missing_later_index", 0, nil, false},
		{"missing_earlier_index", nil, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rows []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row["uuid"] == "assistant-1" {
					delete(row, "apiBlockIndex")
					if test.first != nil {
						row["apiBlockIndex"] = test.first
					}
				}
				if row["uuid"] == "assistant-1b" {
					delete(row, "apiBlockIndex")
					if test.second != nil {
						row["apiBlockIndex"] = test.second
					}
				}
				rows = append(rows, row)
			}
			writeExchangeRows(t, e, rows)
			_, err := e.VerifyArchive(context.Background(), unitArchive(e.s))
			if (err == nil) != test.accept {
				t.Fatalf("archive accepted=%v want=%v", err == nil, test.accept)
			}
		})
	}
}
