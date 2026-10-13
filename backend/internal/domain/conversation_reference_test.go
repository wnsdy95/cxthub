package domain

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestConversationReferenceIdentity(test *testing.T) {
	for _, testCase := range []struct {
		name, stream string
		count        int64
	}{
		{"empty", "", 0},
		{"events", conversationVectorStream, 2},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			manifest, bodies := conversationTestPlan(test, conversationTestEnvelope(test, false), []byte(testCase.stream), testCase.count)
			hash := manifestProofHash(test, manifest)
			var loader func(context.Context, ContentHash) ([]byte, error)
			if len(manifest.Chunks) != 0 {
				loader = manifestProofLoad(bodies)
			}
			proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, loader)
			if err != nil || !proof.Valid() || proof.DocumentRef() != (DocumentRef{Hash: hash, Identity: DocumentIdentityRootV1}) {
				test.Fatal("root reference", proof, err)
			}
			if !proof.Matches(Snapshot{ID: hash, DocHash: hash, DocIdentity: DocumentIdentityRootV1}) || proof.Matches(Snapshot{ID: hash, DocHash: hash}) {
				test.Fatal("reference lost root identity")
			}
			for _, body := range bodies {
				clear(body)
			}
			clear(manifest.Envelope)
			clear(manifest.Chunks)
			if proof.Hash() != hash || !proof.Valid() {
				test.Fatal("reference aliases mutable inputs")
			}
		})
	}
}

func conversationReferenceRepeatedFixture(test *testing.T) (ConversationManifest, map[ContentHash][]byte) {
	test.Helper()
	stream := `{"blocks":[{"text":"` + strings.Repeat("a", 3*ConversationManifestChunkBytes) + `","type":"text"}],"kind":"message","role":"user","seq":0}`
	manifest, bodies := conversationTestPlan(test, conversationTestEnvelope(test, false), []byte(stream), 1)
	if manifest.Chunks[1].Hash != manifest.Chunks[2].Hash {
		test.Fatal("fixture requires repeated occurrences")
	}
	return manifest, bodies
}

func TestConversationReferenceReusableLoaderAndOwnedMetadata(test *testing.T) {
	manifest, bodies := conversationReferenceRepeatedFixture(test)
	hash := manifestProofHash(test, manifest)
	order := append([]ConversationManifestChunk(nil), manifest.Chunks...)
	buffer := make([]byte, ConversationManifestChunkBytes)
	var loaded []ContentHash
	proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
		if len(loaded) == 0 {
			clear(manifest.Envelope)
			clear(manifest.Chunks)
		}
		loaded = append(loaded, chunk)
		length := copy(buffer, bodies[chunk])
		return buffer[:length], nil
	})
	if err != nil || proof.Hash() != hash || len(loaded) != len(order) {
		test.Fatal("reused loader or metadata changed verification", proof, loaded, err)
	}
	for index, chunk := range order {
		if loaded[index] != chunk.Hash {
			test.Fatal("occurrence order changed", loaded)
		}
	}
	clear(buffer)
	if !proof.Valid() || proof.Hash() != hash {
		test.Fatal("reference retained loader buffer")
	}
}

func TestConversationReferenceRejectsTailAndRepeatedOccurrenceTampering(test *testing.T) {
	for _, mode := range []string{"tail", "repeated-occurrence", "length"} {
		test.Run(mode, func(test *testing.T) {
			manifest, bodies := conversationReferenceRepeatedFixture(test)
			hash := manifestProofHash(test, manifest)
			if proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, manifestProofLoad(bodies)); err != nil || !proof.Valid() {
				test.Fatal("initial reference", err)
			}
			seen := make(map[ContentHash]bool)
			calls := 0
			proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
				calls++
				body := bytes.Clone(bodies[chunk])
				if mode == "tail" && calls == len(manifest.Chunks) || mode == "repeated-occurrence" && seen[chunk] {
					body[len(body)-1] ^= 1
				}
				if mode == "length" && calls == len(manifest.Chunks) {
					body = body[:len(body)-1]
				}
				seen[chunk] = true
				return body, nil
			})
			if !errors.Is(err, ErrIntegrity) || proof != (VerifiedDocReference{}) {
				test.Fatal("tampering produced reference", proof, err)
			}
			if mode != "repeated-occurrence" && calls != len(manifest.Chunks) {
				test.Fatal("did not reach final chunk", calls)
			}
		})
	}
}

func TestConversationReferenceRejectsUnverifiedClaims(test *testing.T) {
	manifest, bodies := conversationTestPlan(test, conversationTestEnvelope(test, false), []byte(conversationVectorStream), 2)
	hash := manifestProofHash(test, manifest)
	for _, wrong := range []ContentHash{"invalid", HashContent([]byte("wrong root"))} {
		proof, err := VerifyConversationManifestReference(context.Background(), wrong, manifest, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("loaded bytes for wrong root")
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || proof.Valid() {
			test.Fatal("wrong root reference", proof, err)
		}
	}
	if proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, nil); !errors.Is(err, ErrIntegrity) || proof.Valid() {
		test.Fatal("missing loader reference", proof, err)
	}
	for _, count := range []int64{1, 3} {
		claimed := manifest
		claimed.EventCount = count
		proof, err := VerifyConversationManifestReference(context.Background(), manifestProofHash(test, claimed), claimed, manifestProofLoad(bodies))
		if !errors.Is(err, ErrIntegrity) || proof.Valid() {
			test.Fatal("unverified count reference", proof, err)
		}
	}
}

func TestConversationReferenceSemanticParity(test *testing.T) {
	for _, testCase := range []struct {
		name, stream string
		version      string
		count        int64
		want         error
	}{
		{"valid", conversationVectorStream, "2", 2, nil},
		{"equal-sequence", strings.Replace(conversationVectorStream, `"seq":1`, `"seq":0`, 1), "2", 2, nil},
		{"duplicate-field", strings.Replace(conversationVectorStream, `"seq":1`, `"seq":1,"seq":1`, 1), "2", 2, ErrIntegrity},
		{"sequence-regression", strings.Replace(conversationVectorStream, `"seq":1`, `"seq":-1`, 1), "2", 2, ErrIntegrity},
		{"trailing-comma", conversationVectorStream + ",", "2", 2, ErrIntegrity},
		{"wrong-version", `{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":0}`, "1", 1, ErrUnsupportedCIRVersion},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			envelope := bytes.Replace(conversationTestEnvelope(test, false), []byte(`"cir_version":"2"`), []byte(`"cir_version":"`+testCase.version+`"`), 1)
			manifest, bodies := conversationTestPlan(test, envelope, []byte(testCase.stream), testCase.count)
			hash := manifestProofHash(test, manifest)
			var verifier CanonicalDocVerifier
			document, previousErr := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, manifestProofLoad(bodies))
			proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, manifestProofLoad(bodies))
			if testCase.want == nil {
				if previousErr != nil || err != nil || proof != document.Reference() {
					test.Fatal("valid reference parity", proof, previousErr, err)
				}
			} else if !errors.Is(previousErr, testCase.want) || !errors.Is(err, testCase.want) || proof.Valid() {
				test.Fatal("error contract changed", proof, previousErr, err)
			}
			if testCase.want == ErrUnsupportedCIRVersion && errors.Is(err, ErrIntegrity) {
				test.Fatal("version error relabeled as integrity error", err)
			}
		})
	}
}

func TestConversationReferenceLoaderErrorsAndCancellation(test *testing.T) {
	manifest, bodies := conversationReferenceRepeatedFixture(test)
	hash := manifestProofHash(test, manifest)
	for _, marker := range []error{errors.New("loader unavailable"), ErrNotFound, ErrConversationManifest, context.Canceled, context.DeadlineExceeded} {
		proof, err := VerifyConversationManifestReference(context.Background(), hash, manifest, func(context.Context, ContentHash) ([]byte, error) {
			return nil, marker
		})
		if err != marker || proof != (VerifiedDocReference{}) {
			test.Fatal("loader error changed", proof, err, marker)
		}
	}
	for _, cancelAt := range []int{0, 2, len(manifest.Chunks)} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		if cancelAt == 0 {
			cancel()
		}
		proof, err := VerifyConversationManifestReference(ctx, hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
			calls++
			if calls == cancelAt {
				cancel()
			}
			return bodies[chunk], nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Valid() || calls != cancelAt {
			test.Fatal("canceled verification produced reference", cancelAt, calls, proof, err)
		}
	}
}

func TestConversationReferenceMetadataRemainsUnchanged(test *testing.T) {
	manifest, bodies := conversationTestPlan(test, conversationTestEnvelope(test, false), []byte(conversationVectorStream), 2)
	expected := manifest
	expected.Envelope = bytes.Clone(manifest.Envelope)
	expected.Chunks = append([]ConversationManifestChunk(nil), manifest.Chunks...)
	if _, err := VerifyConversationManifestReference(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies)); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(manifest, expected) {
		test.Fatal("verification mutated caller metadata")
	}
}

func TestConversationReferenceCachedVerificationReadsEveryOccurrence(test *testing.T) {
	manifest, bodies := conversationReferenceRepeatedFixture(test)
	hash := manifestProofHash(test, manifest)
	var verifier CanonicalDocVerifier
	for attempt := 0; attempt < 2; attempt++ {
		calls := 0
		proof, err := verifier.VerifyConversationManifestReference(context.Background(), hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
			calls++
			return bodies[chunk], nil
		})
		if err != nil || !proof.Valid() || calls != len(manifest.Chunks) || len(verifier.events) != 1 {
			test.Fatal("semantic cache skipped current occurrence or lost admission", attempt, calls, len(verifier.events), err)
		}
	}
	before := maps.Clone(verifier.events)
	seen := make(map[ContentHash]bool)
	proof, err := verifier.VerifyConversationManifestReference(context.Background(), hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
		body := bytes.Clone(bodies[chunk])
		if seen[chunk] {
			body[len(body)-1] ^= 1
		}
		seen[chunk] = true
		return body, nil
	})
	if !errors.Is(err, ErrIntegrity) || proof.Valid() || !maps.Equal(verifier.events, before) {
		test.Fatal("warm semantic cache hid repeated current corruption", proof, err)
	}
}

func TestConversationReferenceCachedEventsPreserveFramingAndOrder(test *testing.T) {
	envelope := conversationTestEnvelope(test, false)
	manifest, bodies := conversationTestPlan(test, envelope, []byte(conversationVectorStream), 2)
	var verifier CanonicalDocVerifier
	if _, err := verifier.VerifyConversationManifestReference(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies)); err != nil {
		test.Fatal(err)
	}
	before := maps.Clone(verifier.events)
	boundary := strings.Index(conversationVectorStream, `},{"call_id"`) + 1
	if boundary == 0 || len(before) != 2 {
		test.Fatal("fixture must prime two exact events")
	}
	for _, stream := range []string{
		conversationVectorStream[boundary+1:] + "," + conversationVectorStream[:boundary],
		conversationVectorStream + ",",
		conversationVectorStream[:boundary] + conversationVectorStream[boundary+1:],
	} {
		changed, changedBodies := conversationTestPlan(test, envelope, []byte(stream), 2)
		proof, err := verifier.VerifyConversationManifestReference(context.Background(), manifestProofHash(test, changed), changed, manifestProofLoad(changedBodies))
		if !errors.Is(err, ErrIntegrity) || proof.Valid() || !maps.Equal(verifier.events, before) {
			test.Fatal("cached events authorized invalid complete document", proof, err)
		}
	}
}

func TestConversationReferenceCacheAdmissionRequiresCompleteSuccess(test *testing.T) {
	envelope := conversationTestEnvelope(test, false)
	seed, seedBodies := conversationTestPlan(test, envelope, []byte(`{"kind":"turn","role":"assistant","seq":5}`), 1)
	stream := `{"kind":"turn","role":"user","seq":0},{"blocks":[{"text":"` + strings.Repeat("b", ConversationManifestChunkBytes+17) + `","type":"text"}],"kind":"message","role":"assistant","seq":1}`
	manifest, bodies := conversationTestPlan(test, envelope, []byte(stream), 2)
	for _, mode := range []string{"late-corruption", "late-loader-error", "canceled"} {
		test.Run(mode, func(test *testing.T) {
			var verifier CanonicalDocVerifier
			if _, err := verifier.VerifyConversationManifestReference(context.Background(), manifestProofHash(test, seed), seed, manifestProofLoad(seedBodies)); err != nil {
				test.Fatal(err)
			}
			before := maps.Clone(verifier.events)
			order := append([]canonicalEventKey(nil), verifier.order...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			proof, err := verifier.VerifyConversationManifestReference(ctx, manifestProofHash(test, manifest), manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
				calls++
				body := bytes.Clone(bodies[chunk])
				if calls == len(manifest.Chunks) {
					switch mode {
					case "late-corruption":
						body[len(body)-1] ^= 1
					case "late-loader-error":
						return nil, ErrNotFound
					case "canceled":
						cancel()
					}
				}
				return body, nil
			})
			if err == nil || proof.Valid() || calls != len(manifest.Chunks) || !maps.Equal(verifier.events, before) || !reflect.DeepEqual(verifier.order, order) {
				test.Fatal("failed complete verification admitted pending event proofs", proof, err)
			}
		})
	}
}
