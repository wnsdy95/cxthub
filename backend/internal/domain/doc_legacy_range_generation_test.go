package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func legacyDocRangeGenerationFixture(test testing.TB, count int) (VerifiedSessionDoc, []byte) {
	test.Helper()
	events := canonicalAdmissionEvents(0, count)
	for position := range events {
		sequence := 17 + 7*(position/2)
		locked := &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "opaque-locked-only [cxt seed] Branch-switch context:"}
		event := CIREvent{Kind: EventMessage, Seq: sequence, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "public-message-only"}}}
		switch position % 8 {
		case 0:
			event = CIREvent{Kind: EventReasoning, Seq: sequence, RedactedSummary: " \ufeff\t\n", Locked: locked}
		case 1:
			event.Role, event.CompactSummary = RoleAssistant, true
			event.Blocks[0].Text = " \ufeff[cxt seed] Branch-switch context: public-seed-only"
		case 2:
			event = CIREvent{Kind: EventReasoning, Seq: sequence, RedactedSummary: "public-summary-only", Locked: locked}
		case 3:
			event = CIREvent{Kind: EventToolCall, Seq: sequence, CallID: "original-call", ToolName: "Bash", Input: map[string]any{"command": "private-tool-input-only"}}
		case 4:
			event.Role, event.AgentMessage, event.Locked = RoleAssistant, true, locked
		case 5:
			event = CIREvent{Kind: EventCompaction, Seq: sequence, Locked: locked}
		case 6:
			event = CIREvent{Kind: EventToolResult, Seq: sequence, CallID: "original-call", Output: "private-tool-output-only"}
		case 7:
			event = CIREvent{Kind: EventTurn, Seq: sequence, Role: RoleUser}
		}
		if position == DocRangePageEvents-1 || position == DocRangePageEvents {
			event = CIREvent{Kind: EventReasoning, Seq: sequence, Locked: locked}
		}
		events[position] = event
	}
	for position := 0; position < len(events)/2; position++ {
		other := len(events) - position - 1
		events[position], events[other] = events[other], events[position]
	}
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"original-model"}}, Events: events}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	verified, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
	if err != nil || !verified.Valid() || verified.DocumentRef().Identity != DocumentIdentityLegacy {
		test.Fatalf("legacy fixture verification: %v", err)
	}
	return verified, raw
}

func legacyDocRangeGenerationStreamFixture(test testing.TB, original []byte, width int) (VerifiedLegacyStream, *legacyTestStream) {
	test.Helper()
	envelope, events := chunkVerifierParts(test, original)
	input := chunkVerifierPlan(envelope, events, width)
	spool := &legacyTestStream{}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(original), input.Manifest, chunkVerifierLoad(input), spool)
	if err != nil || !proof.Valid() || proof.Reference().DocumentRef() != (DocumentRef{Hash: HashContent(original), Identity: DocumentIdentityLegacy}) || !bytes.Equal(spool.body, events) {
		test.Fatalf("legacy streamed fixture verification: %v", err)
	}
	return proof, spool
}

func TestLegacyDocRangeGenerationStreamedRejectsInvalidProof(test *testing.T) {
	_, original := legacyDocRangeGenerationFixture(test, 1)
	valid, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
	for name, invalidate := range map[string]func(*VerifiedLegacyStream){
		"zero":              func(proof *VerifiedLegacyStream) { *proof = VerifiedLegacyStream{} },
		"missing-reference": func(proof *VerifiedLegacyStream) { proof.reference = VerifiedDocReference{} },
		"missing-source":    func(proof *VerifiedLegacyStream) { proof.source = nil },
		"wrong-format":      func(proof *VerifiedLegacyStream) { proof.manifest.Format = ChunkFormatV1 },
		"root-identity":     func(proof *VerifiedLegacyStream) { proof.reference.identity = DocumentIdentityRootV1 },
		"unknown-identity":  func(proof *VerifiedLegacyStream) { proof.reference.identity = "future" },
	} {
		test.Run(name, func(test *testing.T) {
			spool.readTransform = func(body []byte) []byte {
				test.Fatal("invalid streamed proof read the spool")
				return body
			}
			proof := valid
			invalidate(&proof)
		})
	}
}

func TestLegacyDocRangeGenerationRootVersion2WireCompatibility(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			generation, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, count))
			if generation.Version != 2 || generation.SourceIdentity != nil || generation.Identity() != DocumentIdentityRootV1 {
				test.Fatal("root generation changed its implicit version 2 identity")
			}
			encoded, err := generation.Encode()
			if err != nil || bytes.Contains(encoded, []byte(`"source_identity"`)) {
				test.Fatalf("version 2 acquired source_identity: %s, %v", encoded, err)
			}
			_ = docRangeGenerationJSON(test, struct {
				Version     int                   `json:"version"`
				Algorithm   int                   `json:"algorithm"`
				Hash        ContentHash           `json:"hash"`
				Envelope    json.RawMessage       `json:"envelope"`
				EventCount  int64                 `json:"event_count"`
				StreamBytes int64                 `json:"stream_bytes"`
				EventPages  DocRangeDirectoryRoot `json:"event_pages"`
				ChunkPages  DocRangeDirectoryRoot `json:"chunk_pages"`
			}{generation.Version, generation.Algorithm, generation.Hash, generation.Envelope, generation.EventCount, generation.StreamBytes, generation.EventPages, generation.ChunkPages})
			identity := DocumentIdentityLegacy
			generation.SourceIdentity = &identity
			if err := generation.Validate(generation.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("version 2 accepted an explicit legacy source identity: %v", err)
			}
		})
	}
}
