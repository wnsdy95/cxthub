package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDocVisibleReplaySelectorAcceptsExactTerminalUnits(test *testing.T) {
	for _, total := range []int{0, 1, 100, MaxConversationDocumentBytes} {
		for _, inherited := range []bool{false, true} {
			selector := visibleTestSelector()
			selector.Start, selector.Next, selector.Total, selector.Unit = total, total, total, 0
			if inherited {
				selector.Base, selector.Inherited = selector.Hash, total
			}
			encoded, err := selector.Encode()
			if err != nil {
				test.Fatal("terminal selector rejected", selector, err)
			}
			decoded, err := DecodeDocVisibleSelector(encoded)
			if err != nil || decoded != selector {
				test.Fatal("terminal selector changed", decoded, selector, err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				test.Fatal(err)
			}
			for _, changed := range []string{
				" " + string(raw), string(raw) + "\n",
				strings.Replace(string(raw), `"unit":0`, `"unit":0,"unit":0`, 1),
				strings.Replace(string(raw), `"unit":0`, `"unit":0,"unknown":0`, 1),
				strings.Replace(string(raw), `"unit":0`, `"unit":0.0`, 1),
			} {
				if _, err := DecodeDocVisibleSelector(base64.RawURLEncoding.EncodeToString([]byte(changed))); !errors.Is(err, ErrValidation) {
					test.Fatal("noncanonical terminal selector accepted", changed, err)
				}
			}
		}
	}
}

func TestDocVisibleReplaySelectorPreservesOtherConstraints(test *testing.T) {
	terminal := visibleTestSelector()
	terminal.Start, terminal.Next, terminal.Unit = terminal.Total, terminal.Total, 0
	for _, scenario := range []struct {
		name   string
		change func(*DocVisibleSelector)
	}{
		{"fold at total", func(selector *DocVisibleSelector) { selector.Purpose = "fold" }},
		{"nonterminal start", func(selector *DocVisibleSelector) { selector.Start-- }},
		{"nonterminal next", func(selector *DocVisibleSelector) { selector.Next-- }},
		{"nonzero unit", func(selector *DocVisibleSelector) { selector.Unit = 1 }},
		{"negative unit", func(selector *DocVisibleSelector) { selector.Unit = -1 }},
		{"negative total", func(selector *DocVisibleSelector) { selector.Total, selector.Start, selector.Next = -1, -1, -1 }},
		{"excess total", func(selector *DocVisibleSelector) {
			selector.Total = MaxConversationDocumentBytes + 1
			selector.Start, selector.Next = selector.Total, selector.Total
		}},
		{"negative start", func(selector *DocVisibleSelector) { selector.Start = -1 }},
		{"excess start", func(selector *DocVisibleSelector) { selector.Start++ }},
		{"negative next", func(selector *DocVisibleSelector) { selector.Next = -1 }},
		{"excess next", func(selector *DocVisibleSelector) { selector.Next++ }},
		{"negative inherited", func(selector *DocVisibleSelector) { selector.Inherited = -1 }},
		{"excess inherited", func(selector *DocVisibleSelector) {
			selector.Base, selector.Inherited = selector.Hash, selector.Total+1
		}},
		{"inherited without base", func(selector *DocVisibleSelector) { selector.Inherited = 1 }},
		{"invalid version", func(selector *DocVisibleSelector) { selector.Version = 0 }},
		{"invalid purpose", func(selector *DocVisibleSelector) { selector.Purpose = "replay" }},
		{"invalid repo", func(selector *DocVisibleSelector) { selector.Repo = "bad" }},
		{"invalid hash", func(selector *DocVisibleSelector) { selector.Hash = "bad" }},
		{"invalid base", func(selector *DocVisibleSelector) { selector.Base = "bad" }},
		{"invalid generation", func(selector *DocVisibleSelector) { selector.Generation = "" }},
		{"invalid view", func(selector *DocVisibleSelector) { selector.View = "raw" }},
		{"empty limit", func(selector *DocVisibleSelector) { selector.Limit = 0 }},
		{"excess limit", func(selector *DocVisibleSelector) { selector.Limit = 101 }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			selector := terminal
			scenario.change(&selector)
			if _, err := selector.Encode(); !errors.Is(err, ErrValidation) {
				test.Fatal("invalid selector encoded", selector, err)
			}
			raw, err := canonicalJSON(selector)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := DecodeDocVisibleSelector(base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrValidation) {
				test.Fatal("invalid selector decoded", selector, err)
			}
		})
	}
	for _, purpose := range []string{"units", "fold"} {
		selector := visibleTestSelector()
		selector.Purpose = purpose
		if _, err := selector.Encode(); err != nil {
			test.Fatal("nonterminal selector rejected", selector, err)
		}
		selector.Total, selector.Start, selector.Next, selector.Unit = 0, 0, 0, 1
		if _, err := selector.Encode(); !errors.Is(err, ErrValidation) {
			test.Fatal("nonzero empty unit accepted", selector, err)
		}
		selector.Unit, selector.Purpose = 0, "fold"
		if _, err := selector.Encode(); !errors.Is(err, ErrValidation) {
			test.Fatal("empty fold accepted", selector, err)
		}
	}
}

func TestDocVisibleReplayCursorIsRequiredJSONString(test *testing.T) {
	for _, cursor := range []string{"", "canonical-replay-selector"} {
		raw, err := json.Marshal(DocVisiblePage{ReplayCursor: cursor})
		if err != nil {
			test.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			test.Fatal(err)
		}
		var decoded string
		if err := json.Unmarshal(fields["replay_cursor"], &decoded); err != nil || decoded != cursor {
			test.Fatal("replay cursor missing or not a string", string(raw), err)
		}
	}
}
