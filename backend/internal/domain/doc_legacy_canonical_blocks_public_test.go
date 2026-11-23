package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func legacyCanonicalBlocksPublicDocument(test *testing.T, count int, nested bool) []byte {
	test.Helper()
	blocks := make([]ContentBlock, count)
	for ordinal := range blocks {
		blocks[ordinal] = ContentBlock{Type: "text", Text: fmt.Sprintf("block-%04d: \ud55c🙂<&>\n\"\\\u2028\u2029", ordinal)}
	}
	message := CIREvent{
		Kind: EventMessage, Seq: 7, ID: "large-message", Role: RoleAssistant, Blocks: blocks,
		AgentMessage: true, AgentAuthor: "worker", AgentRecipient: "parent",
		Locked:           &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "after-blocks<&>"},
		ProviderMetadata: &ProviderMetadata{TurnID: "metadata-after-blocks"},
	}
	first := message
	if nested {
		first = CIREvent{Kind: EventCompaction, Seq: 7, ReplacementComplete: true, Replacement: []CIREvent{
			message,
			{Kind: EventToolCall, Seq: -2, CallID: "before-blocks", ToolName: "test", Input: legacyKeyOrderNestedFixture(19)},
			{Kind: EventCompaction, Seq: 7, ReplacementComplete: true, Replacement: []CIREvent{
				{Kind: EventMessage, Seq: 4, ID: "later", Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "nested-tail"}}},
				{Kind: EventMessage, Seq: -1, ID: "tie-first", Role: RoleUser},
				{Kind: EventToolResult, Seq: -1, CallID: "tie-second", Output: legacyKeyOrderNestedFixture(11)},
			}},
		}}
	}
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: []CIREvent{
		first,
		{Kind: EventToolResult, Seq: -3, CallID: "following-event", Output: legacyKeyOrderNestedFixture(23),
			ProviderMetadata: &ProviderMetadata{TurnID: "following-metadata"}},
	}}
	raw, err := json.Marshal(document)
	if err != nil {
		test.Fatal(err)
	}
	return append(raw, bytes.Repeat([]byte(" \t\r\n"), ConversationManifestChunkBytes/2+1)...)
}
