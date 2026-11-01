package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRangeEnvelopeVersionCompatibility(tests *testing.T) {
	for _, version := range []string{"", CIRVersionV1, CIRVersionV2} {
		for _, count := range []int{0, 3, DocRangePageEvents + 3} {
			tests.Run(fmt.Sprintf("version=%q/events=%d", version, count), func(test *testing.T) {
				document := CIRDocument{Envelope: CIREnvelope{CIRVersion: version, SourceProvider: ProviderCodex, Fidelity: FidelityFull}}
				for ordinal := 0; ordinal < count; ordinal++ {
					text := fmt.Sprintf("legacy prompt %d", ordinal)
					if count > DocRangePageEvents && ordinal == 0 {
						text += strings.Repeat("x", ConversationManifestChunkBytes+17)
					}
					document.Events = append(document.Events, CIREvent{Kind: EventMessage, Seq: 17 + ordinal*3, Role: RoleUser,
						Blocks: []ContentBlock{{Type: "text", Text: text}}})
				}
				raw, err := CanonicalBytes(document)
				if err != nil {
					test.Fatal(err)
				}
				hash := HashContent(raw)
				var verifier CanonicalDocVerifier
				proof, err := verifier.Verify(context.Background(), hash, raw)
				if err != nil {
					test.Fatal("canonical legacy proof rejected", err)
				}
				envelope, stream := chunkVerifierParts(test, raw)
				original := chunkVerifierPlan(envelope, stream, ChunkTarget)
				streamed, _ := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
				for _, variant := range []struct {
					name    string
					version int
					build   func() (DocRangeGeneration, []DocRangeObject, error)
				}{
					{"monolithic", 3, func() (DocRangeGeneration, []DocRangeObject, error) {
						return BuildLegacyDocRangeGeneration(context.Background(), proof)
					}},
					{"streamed", 3, func() (DocRangeGeneration, []DocRangeObject, error) {
						return BuildStreamedLegacyDocRangeGeneration(context.Background(), streamed)
					}},
					{"original_bytes", 4, func() (DocRangeGeneration, []DocRangeObject, error) {
						return BuildStreamedLegacyDocByteRangeGeneration(context.Background(), streamed, original.Manifest)
					}},
					{"derived_bytes", 5, func() (DocRangeGeneration, []DocRangeObject, error) {
						return BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
					}},
				} {
					test.Run(variant.name, func(test *testing.T) {
						generation, objects, err := variant.build()
						if err != nil {
							test.Fatal("verified legacy source could not prepare a range", err)
						}
						if generation.Version != variant.version || generation.Algorithm != 1 || generation.Identity() != DocumentIdentityLegacy ||
							generation.Hash != hash || !bytes.Equal(generation.Envelope, envelope) || generation.EventCount != int64(count) {
							test.Fatal("legacy envelope, identity or storage contract changed", generation)
						}
						encoded, err := generation.Encode()
						if err != nil {
							test.Fatal(err)
						}
						decoded, err := DecodeDocRangeGeneration(hash, HashContent(encoded), encoded)
						if err != nil || !reflect.DeepEqual(generation, decoded) {
							test.Fatal("reader rejected its original generation", err)
						}
						bodies := make(map[ContentHash][]byte, len(objects))
						for _, object := range objects {
							if HashContent(object.Bytes) != object.Hash || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangePageBytes {
								test.Fatal("invalid range metadata object", object.Hash)
							}
							if previous, exists := bodies[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
								test.Fatal("inconsistent metadata object", object.Hash)
							}
							bodies[object.Hash] = object.Bytes
						}
						load := docRangeGenerationLoad(bodies)
						var entries []DocRangeEntry
						for position := 0; position < decoded.EventPages.Count; position++ {
							pageHash, err := ResolveDocRangeLeaf(context.Background(), decoded.EventPages, position, load)
							if err != nil {
								test.Fatal(err)
							}
							body, err := load(context.Background(), pageHash)
							if err != nil {
								test.Fatal(err)
							}
							page, err := decoded.DecodePage(position, pageHash, body)
							if err != nil {
								test.Fatal("prepared page lost legacy events", err)
							}
							entries = append(entries, page...)
						}
						if len(entries) != count {
							test.Fatal("prepared pages lost legacy events", len(entries), count)
						}
						for ordinal, entry := range entries {
							if entry.Seq != document.Events[ordinal].Seq || entry.Role != string(RoleUser) {
								test.Fatal("legacy event metadata changed", entry)
							}
						}
						if count > DocRangePageEvents && variant.version >= 4 && decoded.ChunkCount() < 2 {
							test.Fatal("large compatibility fixture did not cross a chunk boundary")
						}
					})
				}
				if !bytes.Equal(proof.Bytes(), raw) || proof.Hash() != hash {
					test.Fatal("generation rewrote the original document")
				}
			})
		}
	}
}

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
