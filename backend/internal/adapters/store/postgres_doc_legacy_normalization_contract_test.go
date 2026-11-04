//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPostgresLegacyNormalizationAcceptedContracts(test *testing.T) {
	versionOneTurn := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
	}
	fixtures := []struct {
		name     string
		raw      string
		expected domain.CIRDocument
	}{
		{
			name: "duplicate-events-reset-reused-and-shortened-elements",
			raw:  `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":9,"role":"assistant","agent_message":true,"agent_author":"discarded","blocks":[{"type":"text","text":"discarded"}]},{"kind":"reasoning","seq":10,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"discarded"}}],"events":[{"kind":"turn","seq":-1,"role":"system"}],"events":[{"kind":"turn","seq":0,"role":"assistant"},{"kind":"message","seq":0,"role":"user","blocks":[]}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
				Events: []domain.CIREvent{
					{Kind: domain.EventTurn, Seq: 0, Role: domain.RoleAssistant},
					{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{}},
				},
			},
		},
		{
			name: "duplicate-blocks-reuse-shortened-backing-and-preserve-null-scalars",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"blocks":[{"text":"updated"}],"blocks":[{"text":null},{"type":null,"text":"tail"}]}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "updated"}, {Type: "text", Text: "tail"}}}},
			},
		},
		{
			name: "duplicate-blocks-null-element-preserves-reused-struct",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"retained"},{"type":"text","text":"discarded"}],"blocks":[null]}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "retained"}}}},
			},
		},
		{
			name: "duplicate-blocks-empty-array-discards-reused-backing",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"discarded"}],"blocks":[],"blocks":[{}]}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{}}}},
			},
		},
		{
			name: "duplicate-blocks-null-array-discards-reused-backing",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"discarded"}],"blocks":null,"blocks":[{}]}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{}}}},
			},
		},
		{
			name: "duplicate-envelope-structs-merge-and-null-preserves-values",
			raw:  `{"envelope":{"cir_version":"2","source_provider":"codex","source_model":"original","context_tokens":7},"envelope":null,"envelope":{"source_model":null,"context_tokens":null,"cwd":"/merged"},"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
					SourceModel: "original", ContextTokens: 7, Cwd: "/merged"},
				Events: []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
			},
		},
		{
			name: "duplicate-envelope-string-slices-reuse-shortened-elements",
			raw:  `{"envelope":{"cir_version":"2","source_models":["first","second"]},"envelope":{"source_models":["updated"]},"envelope":{"source_models":[null,null]},"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2", SourceModels: []string{"updated", "second"}},
				Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
			},
		},
		{
			name: "duplicate-pointer-structs-merge-and-null-scalars-preserve-values",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"reasoning","seq":3,"seq":null,"ts":"original","ts":null,"locked":{"provider":"codex","scheme":"encrypted_content"},"locked":{"provider":null,"blob":"opaque"}}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventReasoning, Seq: 3, TS: "original",
					Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}}},
			},
		},
		{
			name: "duplicate-input-maps-merge-but-output-objects-replace",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_call","seq":1,"call_id":"call","tool_name":"tool","input":{"keep":1,"replace":{"old":1},"clear":"old"},"input":{"replace":{"new":2},"clear":null,"added":3}},{"kind":"tool_result","seq":2,"call_id":"call","output":{"keep":1,"replace":{"old":1}},"output":{"replace":{"new":2},"added":3}}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{
					{Kind: domain.EventToolCall, Seq: 1, CallID: "call", ToolName: "tool",
						Input: map[string]any{"keep": float64(1), "replace": map[string]any{"new": float64(2)}, "clear": nil, "added": float64(3)}},
					{Kind: domain.EventToolResult, Seq: 2, CallID: "call",
						Output: map[string]any{"replace": map[string]any{"new": float64(2)}, "added": float64(3)}},
				},
			},
		},
		{
			name: "null-input-clears-map-and-null-output-uses-union-default",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_call","seq":1,"call_id":"call","tool_name":"tool","input":{"discarded":1},"input":null,"input":{"new":2}},{"kind":"tool_result","seq":2,"call_id":"call","output":{"discarded":1},"output":null}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{
					{Kind: domain.EventToolCall, Seq: 1, CallID: "call", ToolName: "tool", Input: map[string]any{"new": float64(2)}},
					{Kind: domain.EventToolResult, Seq: 2, CallID: "call", Output: ""},
				},
			},
		},
		{
			name:     "overwritten-unknown-event-kind-does-not-fail-final-union",
			raw:      `{"envelope":{"cir_version":"1"},"events":[{"kind":"unknown","seq":9}],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name:     "overwritten-invalid-event-union-does-not-fail-final-document",
			raw:      `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":9,"role":"user","locked":{"provider":"codex","scheme":"encrypted_content","blob":"discarded"}}],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name:     "overwritten-v2-event-does-not-upgrade-final-v1-document",
			raw:      `{"envelope":{"cir_version":"1"},"events":[{"kind":"compaction","seq":9,"replacement":[],"replacement_complete":false}],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			expected: versionOneTurn,
		},
		{
			name: "nested-negative-and-equal-sequences-sort-stably-with-duplicates",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","id":"top-equal-first","seq":2,"role":"user"},{"kind":"compaction","id":"boundary","seq":0,"replacement_complete":true,"replacement":[{"kind":"turn","id":"inner-equal-first","seq":-1,"role":"user"},{"kind":"compaction","id":"nested","seq":-3,"replacement_complete":true,"replacement":[{"kind":"turn","id":"deep-equal-first","seq":-1,"role":"assistant"},{"kind":"turn","id":"deep-negative","seq":-4,"role":"system"},{"kind":"turn","id":"deep-equal-second","seq":-1,"role":"developer"}]},{"kind":"turn","id":"inner-equal-second","seq":-1,"role":"assistant"},{"kind":"turn","id":"inner-later","seq":3,"role":"developer"}]},{"kind":"turn","id":"top-negative","seq":-2,"role":"system"},{"kind":"turn","id":"top-equal-second","seq":2,"role":"developer"},{"kind":"turn","id":"top-equal-first","seq":2,"role":"user"}]}`,
			expected: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{
					{Kind: domain.EventTurn, ID: "top-negative", Seq: -2, Role: domain.RoleSystem},
					{Kind: domain.EventCompaction, ID: "boundary", Seq: 0, ReplacementComplete: true, Replacement: []domain.CIREvent{
						{Kind: domain.EventCompaction, ID: "nested", Seq: -3, ReplacementComplete: true, Replacement: []domain.CIREvent{
							{Kind: domain.EventTurn, ID: "deep-negative", Seq: -4, Role: domain.RoleSystem},
							{Kind: domain.EventTurn, ID: "deep-equal-first", Seq: -1, Role: domain.RoleAssistant},
							{Kind: domain.EventTurn, ID: "deep-equal-second", Seq: -1, Role: domain.RoleDeveloper},
						}},
						{Kind: domain.EventTurn, ID: "inner-equal-first", Seq: -1, Role: domain.RoleUser},
						{Kind: domain.EventTurn, ID: "inner-equal-second", Seq: -1, Role: domain.RoleAssistant},
						{Kind: domain.EventTurn, ID: "inner-later", Seq: 3, Role: domain.RoleDeveloper},
					}},
					{Kind: domain.EventTurn, ID: "top-equal-first", Seq: 2, Role: domain.RoleUser},
					{Kind: domain.EventTurn, ID: "top-equal-second", Seq: 2, Role: domain.RoleDeveloper},
					{Kind: domain.EventTurn, ID: "top-equal-first", Seq: 2, Role: domain.RoleUser},
				},
			},
		},
	}
	var storage PostgresStore
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			proof := legacyMonolithicVerifierProof(test, fixture.expected)
			raw := []byte(fixture.raw)
			rawHash := domain.HashContent(raw)
			if rawHash == proof.Hash() {
				test.Fatal("literal fixture must require typed normalization")
			}
			for _, testCase := range []legacyMonolithicVerifierCase{
				{name: "normalized-hash", raw: raw, hash: proof.Hash(), expected: proof},
				{name: "raw-hash", raw: raw, hash: rawHash, wantErr: domain.ErrIntegrity},
			} {
				test.Run(testCase.name, func(test *testing.T) {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				})
			}
		})
	}
}

func TestPostgresLegacyNormalizationRejectedContracts(test *testing.T) {
	versionOneTurn := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1"},
		Events:   []domain.CIREvent{{Kind: domain.EventTurn, Seq: 1, Role: domain.RoleUser}},
	}
	fixtures := []struct {
		name    string
		raw     string
		claimed domain.CIRDocument
	}{
		{
			name:    "duplicate-events-null-element-resets-custom-event",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}],"events":[null]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "duplicate-events-missing-kind-does-not-inherit-reused-event",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}],"events":[{"seq":1}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "overwritten-events-retain-earlier-sequence-type-error",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":"invalid","role":"user"}],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "overwritten-events-retain-earlier-block-type-error",
			raw:     `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":9,"role":"user","blocks":[{"type":"text","text":7}]}],"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			claimed: versionOneTurn,
		},
		{
			name:    "duplicate-envelope-retains-earlier-version-type-error",
			raw:     `{"envelope":{"cir_version":1},"envelope":{"cir_version":"1"},"events":[{"kind":"turn","seq":1,"role":"user"}]}`,
			claimed: versionOneTurn,
		},
		{
			name: "duplicate-blocks-retain-earlier-text-type-error",
			raw:  `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":7}],"blocks":[{"type":"text","text":"replacement"}]}]}`,
			claimed: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "replacement"}}}},
			},
		},
		{
			name: "overwritten-replacement-retains-earlier-nested-type-error",
			raw:  `{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","seq":1,"replacement_complete":true,"replacement":[{"kind":"turn","seq":"invalid","role":"user"}],"replacement":[]}]}`,
			claimed: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2"},
				Events: []domain.CIREvent{{Kind: domain.EventCompaction, Seq: 1,
					ReplacementComplete: true, Replacement: []domain.CIREvent{}}},
			},
		},
		{
			name: "duplicate-input-retains-earlier-map-type-error",
			raw:  `{"envelope":{"cir_version":"1"},"events":[{"kind":"tool_call","seq":1,"call_id":"call","tool_name":"tool","input":"invalid","input":{}}]}`,
			claimed: domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1"},
				Events: []domain.CIREvent{{Kind: domain.EventToolCall, Seq: 1,
					CallID: "call", ToolName: "tool", Input: map[string]any{}}},
			},
		},
	}
	var storage PostgresStore
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			proof := legacyMonolithicVerifierProof(test, fixture.claimed)
			raw := []byte(fixture.raw)
			rawHash := domain.HashContent(raw)
			if rawHash == proof.Hash() {
				test.Fatal("literal fixture must differ from the claimed typed document")
			}
			for _, testCase := range []legacyMonolithicVerifierCase{
				{name: "normalized-hash", raw: raw, hash: proof.Hash(), wantErr: domain.ErrIntegrity},
				{name: "raw-hash", raw: raw, hash: rawHash, wantErr: domain.ErrIntegrity},
			} {
				test.Run(testCase.name, func(test *testing.T) {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				})
			}
		})
	}
}
