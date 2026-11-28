package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestRangeEnvelopeCompatibilityDoesNotRelaxRootOrCanonicalBytes(tests *testing.T) {
	hash := HashContent([]byte("original legacy identity"))
	legacy := DocumentIdentityLegacy
	for _, version := range []string{"", CIRVersionV1, CIRVersionV2} {
		tests.Run(fmt.Sprintf("version=%q", version), func(test *testing.T) {
			envelope, err := canonicalJSON(CIREnvelope{CIRVersion: version})
			if err != nil {
				test.Fatal(err)
			}
			root := DocRangeGeneration{Version: 2, Algorithm: 1, Hash: hash, Envelope: envelope}
			_, rootErr := NewConversationManifest(envelope, nil, 0)
			if version == "" {
				if !errors.Is(root.Validate(hash), ErrIntegrity) || !errors.Is(rootErr, ErrConversationManifest) {
					test.Fatal("implicit v1 was admitted as RootV1", rootErr)
				}
			} else if root.Validate(hash) != nil || rootErr != nil {
				test.Fatal("explicit root version was changed", rootErr)
			}
			generation := root
			generation.Version, generation.SourceIdentity = 3, &legacy
			for _, invalid := range [][]byte{
				append([]byte(" "), envelope...),
				bytes.Replace(envelope, []byte(`"cir_version":`), []byte(`"cir_version" :`), 1),
				bytes.Replace(envelope, []byte(`"cir_version":"`+version+`",`), nil, 1),
				bytes.Replace(envelope, []byte(`"cir_version":`), []byte(`"unknown":true,"cir_version":`), 1),
				bytes.Replace(envelope, []byte(`"cir_version":`), []byte(`"cir_version":"1","cir_version":`), 1),
				[]byte(`{}`), []byte(`null`), []byte(`[]`),
				bytes.Replace(envelope, []byte(`"cir_version":"`+version+`"`), []byte(`"cir_version":"99"`), 1),
				bytes.Replace(envelope, []byte(`"cir_version":"`+version+`"`), []byte(`"cir_version":null`), 1),
				bytes.Replace(envelope, []byte(`"cir_version":"`+version+`"`), []byte(`"cir_version":1`), 1),
			} {
				generation.Envelope = json.RawMessage(invalid)
				if !errors.Is(generation.Validate(hash), ErrIntegrity) {
					test.Fatal("noncanonical or unsupported legacy envelope accepted", string(invalid))
				}
			}
			if version == CIRVersionV1 {
				generation.Envelope = bytes.Replace(envelope, []byte(`"cir_version":"1"`), []byte(`"cir_version":"\u0031"`), 1)
				if !errors.Is(generation.Validate(hash), ErrIntegrity) {
					test.Fatal("alternate escaped legacy envelope accepted")
				}
			}
		})
	}
}

func TestLegacyImplicitEnvelopeRejectsPresentFalseV2Field(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{SourceProvider: ProviderCodex}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 17, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "original prompt"}}},
	}}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{`"agent_message":false,`, `"replacement_complete":false,`} {
		changed := bytes.Replace(raw, []byte(`"kind":"message"`), []byte(field+`"kind":"message"`), 1)
		var verifier CanonicalDocVerifier
		proof, err := verifier.Verify(context.Background(), HashContent(changed), changed)
		if !errors.Is(err, ErrUnsupportedCIRVersion) || proof.Valid() {
			test.Fatal("present false v2 field was accepted under implicit v1", field, err)
		}
	}
}

func TestLegacyEnvelopeImplicitVersionStillRejectsV2EventFields(tests *testing.T) {
	for _, event := range []CIREvent{
		{Kind: EventCompaction, Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}},
		{Kind: EventMessage, Role: RoleAssistant, AgentMessage: true},
		{Kind: EventMessage, Role: RoleAssistant, AgentMessage: true, Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}},
	} {
		tests.Run(string(event.Kind)+fmt.Sprint(event.AgentMessage), func(test *testing.T) {
			document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex}, Events: []CIREvent{event}}
			raw, err := CanonicalBytes(document)
			if err != nil {
				test.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte(`"cir_version":"2"`), []byte(`"cir_version":""`), 1)
			var verifier CanonicalDocVerifier
			proof, err := verifier.Verify(context.Background(), HashContent(raw), raw)
			if !errors.Is(err, ErrUnsupportedCIRVersion) || proof.Valid() {
				test.Fatal("raw proof accepted downgraded v2 event", err)
			}
			envelope, stream := chunkVerifierParts(test, raw)
			plan := chunkVerifierPlan(envelope, stream, ChunkTarget)
			proof, err = verifier.VerifyChunks(context.Background(), HashContent(raw), plan.Manifest, chunkVerifierLoad(plan))
			if !errors.Is(err, ErrUnsupportedCIRVersion) || proof.Valid() {
				test.Fatal("chunked proof accepted downgraded v2 event", err)
			}
			streamed, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), plan.Manifest, chunkVerifierLoad(plan), &legacyTestStream{})
			if !errors.Is(err, ErrUnsupportedCIRVersion) || streamed.Valid() {
				test.Fatal("streamed proof accepted downgraded v2 event", err)
			}
		})
	}
}
