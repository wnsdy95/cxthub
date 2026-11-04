package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func visibleTestSelector() DocVisibleSelector {
	return DocVisibleSelector{Version: 1, Purpose: "units", Repo: HashContent([]byte("repo")), Hash: HashContent([]byte("doc")), Generation: HashContent([]byte("generation")), View: "chat", Start: 0, Total: 100, Unit: 2, Next: 20, Limit: 20}
}

func TestDocVisibleSelectorCanonicalAndBounded(t *testing.T) {
	selector := visibleTestSelector()
	encoded, err := selector.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDocVisibleSelector(encoded)
	if err != nil || decoded != selector {
		t.Fatal(decoded, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{
		" " + string(raw), string(raw) + "\n", strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1,"unknown":0`, 1), strings.Replace(string(raw), `"version":1`, `"version":1.0`, 1),
		strings.Replace(string(raw), `"next":20`, `"next":100`, 1), strings.Replace(string(raw), `"start":0`, `"start":-1`, 1),
		strings.Repeat("x", MaxDocVisibleSelectorBytes+1),
	} {
		if got, err := DecodeDocVisibleSelector(base64.RawURLEncoding.EncodeToString([]byte(changed))); !errors.Is(err, ErrValidation) || got != (DocVisibleSelector{}) {
			t.Fatal("noncanonical selector accepted", got, err)
		}
	}
	for _, changed := range []string{"", encoded + "=", encoded + "\n", "*", strings.Repeat("x", 4096)} {
		if _, err := DecodeDocVisibleSelector(changed); !errors.Is(err, ErrValidation) {
			t.Fatal("invalid encoding accepted", err)
		}
	}
	selector.Unit, selector.Next = 3, 30
	relocated, err := selector.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeDocVisibleSelector(relocated); err != nil || got != selector {
		t.Fatal("valid selection was treated as an issued capability", got, err)
	}
}

func TestDocVisibleEventFlagsSurviveFlatMarshal(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		event := DocVisibleEvent{DocViewEvent: DocViewEvent{Event: CIREvent{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "seed"}}}, Index: 7, Bytes: 40, Deferred: deferred}, SeedSummary: true}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["seed_summary"]) != "true" || string(fields["empty_reasoning"]) != "false" || string(fields["event_index"]) != "7" || string(fields["kind"]) != `"message"` {
			t.Fatal(string(raw))
		}
		if deferred && fields["blocks"] != nil || !deferred && fields["blocks"] == nil {
			t.Fatal("flat hydration changed", string(raw))
		}
	}
}
