package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestOrdinaryQuestionAnswerBindingAndLimits(t *testing.T) {
	raw := []byte(`{"questions":[{"question":"Choose?","header":"Choice","options":[{"label":"One","description":"first"},{"label":"Two","description":"second"}],"multiSelect":false}]}`)
	input, err := object(raw)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := parseUserQuestions(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		answers map[string][]string
		valid   bool
	}{
		{"nil", nil, false},
		{"deliberate-unanswered", map[string][]string{}, true},
		{"custom", map[string][]string{"Choose?": {"Other: custom choice"}}, true},
		{"foreign-question", map[string][]string{"Changed?": {"One"}}, false},
		{"multiple-single", map[string][]string{"Choose?": {"One", "Two"}}, false},
		{"unicode-cap", map[string][]string{"Choose?": {strings.Repeat("\U0001f642", 4097)}}, false},
		{"invalid-utf8", map[string][]string{"Choose?": {string([]byte{0xff})}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := questionAnswerInput(input, shown, UserQuestionAnswers{Answers: tc.answers})
			if (err == nil) != tc.valid {
				t.Fatal("incorrect answer admission", err)
			}
		})
	}
	var questions []map[string]any
	_ = json.Unmarshal(input["questions"], &questions)
	questions[0]["kind"] = "text"
	input["questions"], _ = json.Marshal(questions)
	if _, err := parseUserQuestions(input); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unsupported question silently converted", err)
	}
}

func TestOrdinaryToolResultRequiresMatchedTypedBlocks(t *testing.T) {
	for _, raw := range []string{
		`[{"type":"text","text":"not a tool result"}]`,
		`[{"type":"tool_result","content":"missing ID"}]`,
		`[{"type":"tool_result","tool_use_id":"t","content":"x"},{"type":"tool_result","tool_use_id":"t","content":"x"}]`,
		`[{"type":"tool_result","tool_use_id":"t","content":"x","is_error":null}]`,
		`[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{}}]}]`,
		`[{"type":"tool_result","tool_use_id":"t","content":"x","behavior_changing":true}]`,
	} {
		if _, err := ordinaryToolResults(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid tool result accepted")
		}
	}
	ids, err := ordinaryToolResults(json.RawMessage(`[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"bounded output"}],"is_error":true},{"type":"tool_result","tool_use_id":"b","content":"ok"}]`))
	if err != nil || len(ids) != 2 {
		t.Fatal("typed parallel results", err)
	}
}

func TestOrdinaryResultNeverTreatsMalformedUsageAsCompletion(t *testing.T) {
	for _, raw := range []string{`{"inputTokens":-1}`, `{"outputTokens":1.5}`, `{"costUSD":null}`, `{"contextWindow":"1000000"}`} {
		m, q := firstResultFixture()
		q.ordinary = newOrdinaryState(context.Background(), InteractionHandlers{})
		m["num_turns"] = json.RawMessage(`2`)
		m["modelUsage"] = json.RawMessage(`{"model":` + raw + `}`)
		if _, err := firstQuestionResult(m, q); err == nil {
			t.Fatal("malformed usage accepted")
		}
		q.ordinary.cancel()
	}
}

func TestOrdinaryInteractionDiagnosticsAreRedacted(t *testing.T) {
	private := errors.New("PRIVATE_INTERACTION_BODY")
	err := interactionError{cause: private}
	if !errors.Is(err, private) || !errors.Is(err, ErrInteraction) {
		t.Fatal("lost cause")
	}
	values := []any{err, ToolPermissionRequest{Description: private.Error(), Input: json.RawMessage(`{"secret":"PRIVATE_INTERACTION_BODY"}`)}, UserQuestionRequest{Questions: []UserQuestion{{Question: private.Error()}}}, UserQuestionAnswers{Answers: map[string][]string{"q": {private.Error()}}}}
	for _, v := range values {
		if strings.Contains(fmt.Sprintf("%v %+v %#v", v, v, v), private.Error()) {
			t.Fatal("private diagnostic leaked")
		}
	}
}

func TestOrdinaryControlEchoRequiresExactCommittedResponse(t *testing.T) {
	reply := json.RawMessage(`{"subtype":"success","request_id":"permission-1","response":{"behavior":"deny","toolUseID":"tool-1"}}`)
	hash, err := archiveContentHash(reply)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exact", "uncommitted", "changed-decision", "foreign-request", "duplicate", "completed"} {
		t.Run(mode, func(t *testing.T) {
			q := &firstQuestionState{phase: 2, ordinary: newOrdinaryState(context.Background(), InteractionHandlers{})}
			defer q.ordinary.cancel()
			c := &ordinaryControl{settled: true, responseHash: hash}
			q.ordinary.controls["permission-1"] = c
			raw := append(json.RawMessage(nil), reply...)
			switch mode {
			case "uncommitted":
				c.responseHash = ""
			case "changed-decision":
				raw = json.RawMessage(strings.ReplaceAll(string(reply), "deny", "allow"))
			case "foreign-request":
				raw = json.RawMessage(strings.ReplaceAll(string(reply), "permission-1", "permission-2"))
			case "duplicate":
				c.echoed = true
			case "completed":
				q.completed = true
			}
			s := &Session{firstQuestion: q}
			got := s.ordinaryControlEcho(map[string]json.RawMessage{"type": json.RawMessage(`"control_response"`), "response": raw})
			if (got == nil) != (mode == "exact") {
				t.Fatalf("echo classification: %v", got)
			}
		})
	}
}
