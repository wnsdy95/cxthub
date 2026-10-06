package nativeclaude

import (
	"encoding/json"
	"testing"
)

// The pinned host reports its window selector in context summaries, while
// assistant/API usage and archived model attachments use the full API model ID.
func TestWindowSelectorResponseIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, selected, actual string
		valid                  bool
	}{
		{"window selector", "claude-opus-5-5[1m]", "claude-opus-5-5", true},
		{"exact selector", "claude-opus-5-5[1m]", "claude-opus-5-5[1m]", true},
		{"exact model", "claude-opus-5-5", "claude-opus-5-5", true},
		{"model switch", "claude-opus-5-5[1m]", "claude-sonnet-5-5", false},
		{"version switch", "claude-opus-5-5[1m]", "claude-opus-4-6", false},
		{"unknown selector", "claude-opus-5-5[2m]", "claude-opus-5-5", false},
		{"stacked selector", "claude-opus-5-5[1m][1m]", "claude-opus-5-5[1m]", false},
		{"bare alias", "opus[1m]", "opus", false},
		{"empty full name", "claude-[1m]", "claude-", false},
		{"selector added", "claude-opus-5-5", "claude-opus-5-5[1m]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"role": "assistant", "model": tc.actual, "content": []any{map[string]any{"type": "text", "text": "reply"}}})
			message, _ := object(raw)
			_, _, _, err := ordinaryAssistant(message, tc.selected)
			if (err == nil) != tc.valid {
				t.Errorf("assistant valid=%v, error=%v", tc.valid, err)
			}
			result, q := firstResultFixture()
			q.summary.Model = tc.selected
			original := q.summary
			result["modelUsage"], _ = json.Marshal(map[string]any{tc.actual: map[string]any{}})
			_, err = firstQuestionResult(result, q)
			if (err == nil) != tc.valid {
				t.Errorf("result valid=%v, error=%v", tc.valid, err)
			}
			s := &Session{id: "session", firstQuestion: q}
			frame, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": "session", "model": tc.actual})
			fields, _ := object(frame)
			_, err = s.firstQuestionFrame(fields, "system")
			if (err == nil) != tc.valid {
				t.Errorf("init valid=%v, error=%v", tc.valid, err)
			}
			attachment, _ := json.Marshal(map[string]any{"type": "model", "identity": map[string]any{"modelId": tc.actual}, "text": "synthetic model description"})
			_, err = validateOrdinaryAttachment(map[string]json.RawMessage{"attachment": attachment}, "session", "/synthetic", tc.selected)
			if (err == nil) != tc.valid {
				t.Errorf("archive valid=%v, error=%v", tc.valid, err)
			}
			if q.summary != original {
				t.Fatal("response comparison changed selected model/window")
			}
		})
	}
}

func TestWindowSelectorDoesNotMergeMultipleUsageModels(t *testing.T) {
	result, q := firstResultFixture()
	q.summary.Model = "claude-opus-5-5[1m]"
	result["modelUsage"] = json.RawMessage(`{"claude-opus-5-5":{},"claude-opus-5-5[1m]":{}}`)
	if _, err := firstQuestionResult(result, q); err == nil {
		t.Fatal("multiple model accounting accepted")
	}
}
