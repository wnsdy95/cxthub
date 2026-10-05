package nativeclaude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func exchangeMetadataFixture() (*Session, *firstQuestionState, ReferenceReceipt) {
	s := &Session{id: "owned-session", cwd: "/owned/cwd"}
	q := &firstQuestionState{id: "question-id", hash: hashText("question"), bytes: len("question"), archiveMessages: []archiveAssistant{{id: "assistant-old"}, {id: "assistant-final"}}}
	r := ReferenceReceipt{SessionID: s.id, MessageID: "reference-id", PayloadHash: hashText("reference"), UTF8Bytes: len("reference"), NativeContentHash: hashText(nativeReferencePrefix + "reference"), NativeUTF8Bytes: len(nativeReferencePrefix + "reference")}
	return s, q, r
}

func exchangeMetadataRow(t *testing.T, kind string, fields map[string]any) map[string]json.RawMessage {
	t.Helper()
	row := map[string]any{"type": kind, "sessionId": "owned-session"}
	for key, value := range fields {
		row[key] = value
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	m, err := object(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func exchangeCostFields() map[string]any {
	return map[string]any{
		"totalCostUSD": 0.125, "totalAPIDuration": 12.5, "totalAPIDurationWithoutRetries": 10.5,
		"totalToolDuration": 0, "totalLinesAdded": 0, "totalLinesRemoved": 0,
		"totalDuration": 25.5, "startTime": 1791158400000,
		"modelUsage": map[string]any{"native-model": exchangeModelCost()},
	}
}

func exchangeModelCost() map[string]any {
	return map[string]any{"inputTokens": 3, "outputTokens": 2, "cacheReadInputTokens": 1, "cacheCreationInputTokens": 0, "webSearchRequests": 0, "costUSD": 0.125}
}

func TestExchangeMetadataOwnedNativeRows(t *testing.T) {
	s, q, r := exchangeMetadataFixture()
	validate := newExchangeMetadataValidator(s, q, r, nil)
	for _, item := range []struct {
		kind   string
		fields map[string]any
	}{
		{"queue-operation", map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.123Z", "content": "reference", "commandUuid": r.MessageID}},
		{"queue-operation", map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.124Z", "content": nativeReferencePrefix + "reference", "commandUuid": r.MessageID}},
		{"queue-operation", map[string]any{"operation": "dequeue", "timestamp": "2026-10-05T00:00:00.125Z"}},
		{"queue-operation", map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.126Z", "content": "question", "commandUuid": q.id}},
		{"queue-operation", map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.127Z", "content": "question"}},
		{"atis-latch", map[string]any{"atis": "opaque!~", "cwd": s.cwd}},
		{"atis-latch", map[string]any{"atis": "opaque!~"}},
		{"last-prompt", map[string]any{"lastPrompt": "display excerpt"}},
		{"last-prompt", map[string]any{"lastPrompt": "question", "leafUuid": "assistant-final", "explicit": true, "rewound": false}},
		{"cost-state", exchangeCostFields()},
		{"cost-state", exchangeCostFields()},
	} {
		if err := validate(exchangeMetadataRow(t, item.kind, item.fields), item.kind); err != nil {
			t.Fatalf("%s: %v", item.kind, err)
		}
	}
}

func TestExchangeMetadataRejectsUnownedOrActiveSemantics(t *testing.T) {
	queue := func(fields map[string]any) map[string]any {
		out := map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.123Z"}
		for k, v := range fields {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name, kind string
		fields     map[string]any
	}{
		{"foreign-session", "atis-latch", map[string]any{"atis": "", "sessionId": "other"}},
		{"null-session", "atis-latch", map[string]any{"atis": "", "sessionId": nil}},
		{"foreign-cwd", "atis-latch", map[string]any{"atis": "", "cwd": "/other"}},
		{"null-cwd", "atis-latch", map[string]any{"atis": "", "cwd": nil}},
		{"unknown-type", "future", map[string]any{}},
		{"wrong-kind", "atis-latch", map[string]any{"atis": "", "type": "last-prompt"}},
		{"unknown-field", "atis-latch", map[string]any{"atis": "", "instructions": "PRIVATE_MARKER"}},
		{"null-atis", "atis-latch", map[string]any{"atis": nil}},
		{"space-atis", "atis-latch", map[string]any{"atis": "a b"}},
		{"newline-atis", "atis-latch", map[string]any{"atis": "a\nb"}},
		{"unicode-atis", "atis-latch", map[string]any{"atis": "é"}},
		{"large-atis", "atis-latch", map[string]any{"atis": strings.Repeat("a", exchangeMetadataText+1)}},
		{"foreign-content", "queue-operation", queue(map[string]any{"content": "PRIVATE_MARKER"})},
		{"null-content", "queue-operation", queue(map[string]any{"content": nil})},
		{"array-content", "queue-operation", queue(map[string]any{"content": []any{"question"}})},
		{"crossed-content-id", "queue-operation", queue(map[string]any{"content": "question", "commandUuid": "reference-id"})},
		{"foreign-command", "queue-operation", queue(map[string]any{"commandUuid": "foreign"})},
		{"null-command", "queue-operation", queue(map[string]any{"commandUuid": nil})},
		{"remove", "queue-operation", queue(map[string]any{"operation": "remove"})},
		{"reason", "queue-operation", queue(map[string]any{"reason": "unknown ingress"})},
		{"delivery-id", "queue-operation", queue(map[string]any{"deliveryId": "other ingress"})},
		{"timestamp-null", "queue-operation", queue(map[string]any{"timestamp": nil})},
		{"timestamp-date-only", "queue-operation", queue(map[string]any{"timestamp": "2026-10-05"})},
		{"timestamp-impossible", "queue-operation", queue(map[string]any{"timestamp": "2026-02-30T00:00:00.123Z"})},
		{"last-old-assistant", "last-prompt", map[string]any{"leafUuid": "assistant-old"}},
		{"last-question", "last-prompt", map[string]any{"leafUuid": "question-id"}},
		{"last-null", "last-prompt", map[string]any{"leafUuid": nil}},
		{"last-foreign", "last-prompt", map[string]any{"leafUuid": "foreign"}},
		{"last-explicit-no-leaf", "last-prompt", map[string]any{"explicit": true}},
		{"last-explicit-null", "last-prompt", map[string]any{"explicit": nil}},
		{"last-rewound", "last-prompt", map[string]any{"leafUuid": "assistant-final", "rewound": true}},
		{"last-rewound-null", "last-prompt", map[string]any{"rewound": nil}},
		{"last-null-prompt", "last-prompt", map[string]any{"lastPrompt": nil}},
		{"last-large-prompt", "last-prompt", map[string]any{"lastPrompt": strings.Repeat("a", exchangeMetadataText+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, q, r := exchangeMetadataFixture()
			err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, tc.kind, tc.fields), tc.kind)
			if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "PRIVATE_MARKER") {
				t.Fatal("expected redacted protocol error", err)
			}
		})
	}
}

func TestExchangeMetadataReadBoundsAndFrozenIdentity(t *testing.T) {
	s, q, r := exchangeMetadataFixture()
	validate := newExchangeMetadataValidator(s, q, r, nil)
	// Freeze primitive identities, not mutable slices or Session pointers.
	s.id, s.cwd, q.id, q.hash = "changed", "/changed", "changed", "changed"
	q.archiveMessages[len(q.archiveMessages)-1].id = "changed"
	row := exchangeMetadataRow(t, "last-prompt", map[string]any{"leafUuid": "assistant-final", "cwd": "/owned/cwd"})
	for i := 0; i < exchangeMetadataRows; i++ {
		if err := validate(row, "last-prompt"); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	if err := validate(row, "last-prompt"); !errors.Is(err, ErrProtocol) {
		t.Fatal("unbounded metadata rows", err)
	}
	s, q, r = exchangeMetadataFixture()
	validate = newExchangeMetadataValidator(s, q, r, nil)
	atis := exchangeMetadataRow(t, "atis-latch", map[string]any{"atis": ""})
	if err := validate(atis, "atis-latch"); err != nil {
		t.Fatal("empty native latch", err)
	}
	atis["atis"] = json.RawMessage(`"different"`)
	if err := validate(atis, "atis-latch"); !errors.Is(err, ErrProtocol) {
		t.Fatal("conflicting latch", err)
	}
	if err := newExchangeMetadataValidator(s, q, r, nil)(atis, "atis-latch"); err != nil {
		t.Fatal("state leaked across reads", err)
	}
	// Repeated owned queue content can exhaust the aggregate bound even when
	// every individual content hash and row count remains admissible.
	text := strings.Repeat("a", 1<<20)
	r.PayloadHash, r.UTF8Bytes = hashText(text), len(text)
	validate = newExchangeMetadataValidator(s, q, r, nil)
	large := exchangeMetadataRow(t, "queue-operation", map[string]any{"operation": "enqueue", "timestamp": "2026-10-05T00:00:00.000Z", "content": text})
	for i := 0; i < 31; i++ {
		if err := validate(large, "queue-operation"); err != nil {
			t.Fatal("premature aggregate limit", err)
		}
	}
	if err := validate(large, "queue-operation"); !errors.Is(err, ErrProtocol) {
		t.Fatal("aggregate byte limit not enforced", err)
	}
}

func TestExchangeMetadataCostNativeSchema(t *testing.T) {
	s, q, r := exchangeMetadataFixture()
	fields := exchangeCostFields()
	fields["hasUnknownModelCost"] = true
	fields["modelUsage"].(map[string]any)["native-model"].(map[string]any)["thinkingTokens"] = 1.5
	if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); err != nil {
		t.Fatal("native finite fractional numbers/optional fields", err)
	}
	fields = exchangeCostFields()
	fields["modelUsage"] = map[string]any{}
	if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); err != nil {
		t.Fatal("empty native model map", err)
	}
	for _, key := range []string{"totalCostUSD", "totalAPIDuration", "totalAPIDurationWithoutRetries", "totalToolDuration", "totalLinesAdded", "totalLinesRemoved", "totalDuration", "startTime", "modelUsage"} {
		t.Run("missing-"+key, func(t *testing.T) {
			fields := exchangeCostFields()
			delete(fields, key)
			if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
				t.Fatal("missing required native field accepted", err)
			}
		})
	}
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens", "webSearchRequests", "costUSD"} {
		t.Run("model-missing-"+key, func(t *testing.T) {
			fields := exchangeCostFields()
			delete(fields["modelUsage"].(map[string]any)["native-model"].(map[string]any), key)
			if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
				t.Fatal("missing required native model field accepted", err)
			}
		})
	}
}

func TestExchangeMetadataCostRejectsMalformedAndConflicting(t *testing.T) {
	cases := map[string]func(map[string]json.RawMessage){
		"null":                    func(m map[string]json.RawMessage) { m["startTime"] = json.RawMessage(" null ") },
		"string":                  func(m map[string]json.RawMessage) { m["startTime"] = json.RawMessage(`"1"`) },
		"negative":                func(m map[string]json.RawMessage) { m["totalCostUSD"] = json.RawMessage("-1") },
		"overflow":                func(m map[string]json.RawMessage) { m["startTime"] = json.RawMessage("1e309") },
		"native-number-bound":     func(m map[string]json.RawMessage) { m["startTime"] = json.RawMessage("1000000000000001") },
		"native-total-cost-bound": func(m map[string]json.RawMessage) { m["totalCostUSD"] = json.RawMessage("1000000001") },
		"nan":                     func(m map[string]json.RawMessage) { m["totalCostUSD"] = json.RawMessage("NaN") },
		"optional-null":           func(m map[string]json.RawMessage) { m["hasUnknownModelCost"] = json.RawMessage("null") },
		"unknown-field":           func(m map[string]json.RawMessage) { m["capacity"] = json.RawMessage("1000000") },
		"null-model":              func(m map[string]json.RawMessage) { m["modelUsage"] = json.RawMessage(`{"model":null}`) },
		"duplicate-model":         func(m map[string]json.RawMessage) { m["modelUsage"] = json.RawMessage(`{"model":{},"model":{}}`) },
		"duplicate-token": func(m map[string]json.RawMessage) {
			m["modelUsage"] = json.RawMessage(`{"model":{"inputTokens":1,"inputTokens":2}}`)
		},
		"raw-bound": func(m map[string]json.RawMessage) {
			m["startTime"] = append(json.RawMessage(strings.Repeat(" ", exchangeCostBytes)), '1')
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, q, r := exchangeMetadataFixture()
			m := exchangeMetadataRow(t, "cost-state", exchangeCostFields())
			mutate(m)
			if err := newExchangeMetadataValidator(s, q, r, nil)(m, "cost-state"); !errors.Is(err, ErrProtocol) {
				t.Fatal("malformed cost accepted", err)
			}
		})
	}
	for _, key := range []string{"", "bad\nmodel", "bad\u200bmodel", strings.Repeat("a", 513)} {
		s, q, r := exchangeMetadataFixture()
		fields := exchangeCostFields()
		fields["modelUsage"] = map[string]any{key: exchangeModelCost()}
		if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
			t.Fatal("invalid model key accepted", err)
		}
	}
	s, q, r := exchangeMetadataFixture()
	validate := newExchangeMetadataValidator(s, q, r, nil)
	fields := exchangeCostFields()
	if err := validate(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); err != nil {
		t.Fatal(err)
	}
	fields["totalCostUSD"] = 0.25
	if err := validate(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
		t.Fatal("conflicting accounting snapshots", err)
	}
	for _, tokenKey := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"} {
		fields := exchangeCostFields()
		one, two := exchangeModelCost(), exchangeModelCost()
		one[tokenKey], two[tokenKey] = 6e14, 6e14
		fields["modelUsage"] = map[string]any{"one": one, "two": two}
		if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
			t.Fatal("native aggregate token bound", tokenKey, err)
		}
	}
	fields = exchangeCostFields()
	models := map[string]any{}
	for i := 0; i <= exchangeCostModels; i++ {
		models[fmt.Sprintf("model-%d", i)] = exchangeModelCost()
	}
	fields["modelUsage"] = models
	if err := newExchangeMetadataValidator(s, q, r, nil)(exchangeMetadataRow(t, "cost-state", fields), "cost-state"); !errors.Is(err, ErrProtocol) {
		t.Fatal("model-count bound", err)
	}
}

func TestExchangeAttachmentInitialContextAndCalendarDate(t *testing.T) {
	for _, tc := range []struct{ raw, kind string }{
		{`{"type":"session_context","context":{}}`, "session_context"},
		{`{"type":"session_context","context":{"userEmail":"","attachedProject":"synthetic project","gitStatus":"arbitrary native\nmodel-visible text","perforceMode":"enabled"}}`, "session_context"},
		{`{"type":"date","date":"2024-02-29"}`, "date"},
		{`{"type":"date","date":"2000-02-29"}`, "date"},
		{`{"type":"date","date":"2026-10-05"}`, "date"},
	} {
		kind, err := validateExchangeAttachment(json.RawMessage(tc.raw), "owned-session")
		if err != nil || kind != tc.kind {
			t.Fatal("initial attachment rejected", tc.kind, err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{"type":"future"}`,
		`{"type":"session_context"}`,
		`{"type":"session_context","context":null}`,
		`{"type":"session_context","context":{"gitStatus":null}}`,
		`{"type":"session_context","context":{"perforceMode":true}}`,
		`{"type":"session_context","context":{"unknown":"PRIVATE_MARKER"}}`,
		`{"type":"session_context","context":{"gitStatus":"a","gitStatus":"b"}}`,
		`{"type":"session_context","context":{},"changed":false}`,
		`{"type":"session_context","context":{},"reason":"refresh"}`,
		`{"type":"date","date":null}`,
		`{"type":"date","date":"PRIVATE_MARKER"}`,
		`{"type":"date","date":"2026-10-05\nPRIVATE_MARKER"}`,
		`{"type":"date","date":"2026-02-29"}`,
		`{"type":"date","date":"1900-02-29"}`,
		`{"type":"date","date":"0000-01-01"}`,
		`{"type":"date","date":"2026-13-01"}`,
		`{"type":"date","date":"2026-1-01"}`,
		`{"type":"date","date":"2026-10-05","changed":true}`,
		`{"type":"date","date":"2026-10-05","date":"2026-10-06"}`,
		`{"type":"date","date":"2026-10-05","sessionId":"foreign"}`,
		`{"type":"date","date":"2026-10-05"} {}`,
	} {
		if kind, err := validateExchangeAttachment(json.RawMessage(raw), "owned-session"); kind != "" || !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "PRIVATE_MARKER") {
			t.Fatal("expected redacted attachment rejection", kind, err)
		}
	}
	for _, size := range []int{exchangeContextValue, exchangeContextValue + 1} {
		raw, _ := json.Marshal(map[string]any{"type": "session_context", "context": map[string]string{"gitStatus": strings.Repeat("x", size)}})
		_, err := validateExchangeAttachment(raw, "owned-session")
		if (err == nil) != (size == exchangeContextValue) {
			t.Fatal("context byte bound", size, err)
		}
	}
	if _, err := validateExchangeAttachment(json.RawMessage(strings.Repeat(" ", exchangeAttachmentBytes+1)), "owned-session"); !errors.Is(err, ErrProtocol) {
		t.Fatal("raw attachment byte bound", err)
	}
	if _, err := validateExchangeAttachment(json.RawMessage(`{"type":"date","date":"2026-10-05"}`), ""); !errors.Is(err, ErrProtocol) {
		t.Fatal("missing containing session", err)
	}
}
