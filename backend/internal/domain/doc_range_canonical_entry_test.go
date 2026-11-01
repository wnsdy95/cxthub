package domain

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalRangeIndexPreservesGenericMetadataPages(test *testing.T) {
	ctx := context.Background()
	events := canonicalAdmissionEvents(0, 2*DocRangePageEvents+1)
	for ordinal := range events {
		sequence := ordinal/2 - DocRangePageEvents
		switch ordinal % 6 {
		case 0:
			events[ordinal] = CIREvent{Kind: EventMessage, Seq: sequence, Role: RoleUser,
				Blocks: []ContentBlock{{Type: "text", Text: " \ufeff[cxt seed] Branch-switch context: main → work"}}}
		case 1:
			events[ordinal] = CIREvent{Kind: EventReasoning, Seq: sequence, RedactedSummary: " \u2003\ufeff\t"}
		case 2:
			events[ordinal] = CIREvent{Kind: EventToolCall, Seq: sequence, ToolName: strings.Repeat("한🙂", 80), Input: map[string]any{"x": true}}
		case 3:
			events[ordinal] = CIREvent{Kind: EventToolResult, Seq: sequence, Output: "tool output"}
		case 4:
			events[ordinal] = CIREvent{Kind: EventMessage, Seq: sequence, Role: RoleAssistant, CompactSummary: true,
				Blocks: []ContentBlock{{Type: "text", Text: "summary \"\\\n<&>"}}}
		case 5:
			events[ordinal] = CIREvent{Kind: EventTurn, Seq: sequence, Role: Role(strings.Repeat("한🙂", 80))}
		}
	}
	events[DocRangePageEvents+1] = CIREvent{Kind: EventToolResult, Seq: events[DocRangePageEvents+1].Seq, Output: strings.Repeat("x", 1<<20)}
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: events})
	if err != nil {
		test.Fatal(err)
	}
	hash := manifestProofHash(test, manifest)
	var owner CanonicalDocVerifier
	document, err := owner.VerifyConversationManifestDoc(ctx, hash, manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	expectedBuffer := &docRangeMemoryBuffer{}
	expectedBuilder := docRangeBuilder{ctx: ctx, buffer: expectedBuffer, result: VerifiedDocRangeIndex{index: DocRangeIndex{Version: 1, Hash: hash, Manifest: manifest, Pages: []ContentHash{}}}}
	for ordinal, event := range document.chunks.events {
		body, err := document.EventStreamRange(ctx, event.span.offset, event.span.length)
		if err != nil {
			test.Fatal(err)
		}
		if err := expectedBuilder.add(int64(ordinal), body, event.seq); err != nil {
			test.Fatal(err)
		}
	}
	expected, err := expectedBuilder.seal()
	if err != nil {
		test.Fatal(err)
	}
	actualBuffer := &docRangeMemoryBuffer{}
	var verifier CanonicalDocVerifier
	streamed, err := verifier.VerifyConversationRangeIndex(ctx, hash, manifest, manifestProofLoad(bodies), actualBuffer)
	if err != nil {
		test.Fatal(err)
	}
	owned, err := BuildDocRangeIndex(ctx, document)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(expectedBuffer.pages, actualBuffer.pages) || !reflect.DeepEqual(expected.Header(), streamed.Header()) || !reflect.DeepEqual(expected.Header(), owned.Header()) {
		test.Fatal("verified metadata changed page bytes, hashes or header")
	}
	for ordinal, want := range expectedBuffer.pages {
		for _, index := range []VerifiedDocRangeIndex{streamed, owned} {
			got, err := index.Page(ctx, ordinal)
			if err != nil || !bytes.Equal(got, want) {
				test.Fatal("verified metadata read changed ranks/order/page identity", ordinal, err)
			}
		}
	}
}

func TestCanonicalRangeIndexChecksSchemaBeforeVerifiedMetadata(test *testing.T) {
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2},
		Events: []CIREvent{{Kind: EventMessage, Seq: 17, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "original"}}}}})
	if err != nil || len(manifest.Chunks) != 1 {
		test.Fatal("fixture must fit a single authenticated chunk", err)
	}
	for _, needle := range []string{`"kind":"message"`, `"seq":17`} {
		original := bodies[manifest.Chunks[0].Hash]
		replacement := `"kind":"invalid"`
		if needle == `"seq":17` {
			replacement = `"seq":"invalid"`
		}
		changed := bytes.Replace(original, []byte(needle), []byte(replacement), 1)
		if bytes.Equal(changed, original) {
			test.Fatal("fixture mutation missed its target")
		}
		candidate := manifest
		candidate.Chunks = []ConversationManifestChunk{{Hash: HashContent(changed), Bytes: int64(len(changed))}}
		candidate.StreamBytes = int64(len(changed))
		buffer := &docRangeMemoryBuffer{}
		var verifier CanonicalDocVerifier
		index, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(test, candidate), candidate, func(context.Context, ContentHash) ([]byte, error) {
			return changed, nil
		}, buffer)
		if err == nil || index.index.Hash != "" || index.read != nil || len(buffer.pages) != 0 || len(verifier.events) != 0 {
			test.Fatal("authenticated but invalid event reached verified metadata or publication", needle, err)
		}
	}
}
