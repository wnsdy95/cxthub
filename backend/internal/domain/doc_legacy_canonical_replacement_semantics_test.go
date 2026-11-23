package domain

import (
	"testing"
)

func TestLegacyReplacementSemanticsNullEmptyLockedProvider(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		child    string
		expected string
	}{
		{
			name:     "empty-incomplete-boundary",
			child:    `{"kind":"compaction","replacement":[]}`,
			expected: `{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":0}`,
		},
		{
			name:     "empty-complete-boundary",
			child:    `{"kind":"compaction","replacement":[],"replacement_complete":true}`,
			expected: `{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":0}`,
		},
		{
			name:     "absent-boundary-locked-state",
			child:    `{"kind":"compaction","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`,
			expected: `{"kind":"compaction","locked":{"blob":"opaque","provider":"codex","scheme":"encrypted_content"},"seq":0}`,
		},
		{
			name:     "null-boundary-is-not-empty-boundary",
			child:    `{"kind":"compaction","replacement":null,"replacement_complete":false,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`,
			expected: `{"kind":"compaction","locked":{"blob":"opaque","provider":"codex","scheme":"encrypted_content"},"seq":0}`,
		},
		{
			name:     "present-false-agent-message-with-null-blocks",
			child:    `{"kind":"message","agent_message":false,"blocks":null}`,
			expected: `{"agent_message":false,"blocks":[],"kind":"message","role":"","seq":0}`,
		},
		{
			name:     "empty-blocks-and-empty-provider",
			child:    `{"kind":"message","blocks":[],"provider_metadata":{}}`,
			expected: `{"blocks":[],"kind":"message","provider_metadata":{},"role":"","seq":0}`,
		},
		{
			name:     "provider-number-keeps-precision",
			child:    `{"kind":"turn","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"provider-id","extra":"not-emitted"}}`,
			expected: `{"kind":"turn","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"provider-id"},"role":"","seq":0}`,
		},
		{
			name:     "null-tool-result-is-empty-string",
			child:    `{"kind":"tool_result","output":null,"is_error":false}`,
			expected: `{"call_id":"","kind":"tool_result","output":"","seq":0}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` + fixture.child + `]}]}`)
			_ = `[{"kind":"compaction","replacement":[` + fixture.expected + `],"replacement_complete":false,"seq":0}]`
		})
	}
}

func TestLegacyReplacementSemanticsInvalidNestedChild(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		child    string
		deferred bool
	}{
		{name: "unknown-union", child: `{"kind":"unknown"}`, deferred: true},
		{name: "turn-with-null-replacement", child: `{"kind":"turn","replacement":null}`, deferred: true},
		{name: "turn-with-present-false-agent", child: `{"kind":"turn","agent_message":false}`, deferred: true},
		{name: "message-with-empty-author", child: `{"kind":"message","agent_author":""}`, deferred: true},
		{name: "message-with-locked-without-agent", child: `{"kind":"message","locked":{}}`, deferred: true},
		{name: "null-provider", child: `{"kind":"turn","provider_metadata":null}`, deferred: true},
		{name: "null-locked", child: `{"kind":"reasoning","locked":null}`, deferred: true},
		{name: "null-boundary-without-state", child: `{"kind":"compaction","replacement":null}`, deferred: true},
		{name: "empty-boundary-and-locked", child: `{"kind":"compaction","replacement":[],"locked":{}}`, deferred: true},
		{name: "wrong-provider-type", child: `{"kind":"turn","provider_metadata":{"turn_id":7}}`},
		{name: "wrong-block-type", child: `{"kind":"message","blocks":[{"text":7}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` +
				`{"kind":"compaction","replacement":[{"kind":"turn","role":"user"},` + fixture.child + `]}]}]}`)
		})
	}
}

func TestLegacyReplacementSemanticsLateTypedErrorSurvivesEventsReset(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		child string
	}{
		{name: "provider-type", child: `{"kind":"turn","provider_metadata":{"turn_id":7}}`},
		{name: "provider-number", child: `{"kind":"turn","provider_metadata":{"create_time":"not-a-number"}}`},
		{name: "locked-type", child: `{"kind":"reasoning","locked":{"blob":7}}`},
		{name: "block-type", child: `{"kind":"message","blocks":[{"text":7}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` +
				`{"kind":"turn","agent_message":false},{"kind":"compaction","replacement":[` + fixture.child + `]}]}],` +
				`"events":[{"kind":"turn","id":"cannot-repair-wire-error"}]}`)
		})
	}
}
