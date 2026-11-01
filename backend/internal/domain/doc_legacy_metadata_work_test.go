package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyMetadataWorkStream struct {
	*legacyTestStream
	test  testing.TB
	ctx   context.Context
	reads []canonicalSpan
}

func (source *legacyMetadataWorkStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("source read retained an earlier request context")
	}
	source.reads = append(source.reads, canonicalSpan{offset: offset, length: length})
	return source.legacyTestStream.Read(ctx, offset, length)
}

func legacyMetadataWorkPending(test testing.TB, state *LegacyRangeMetadataPreparation) {
	test.Helper()
	metadata, err := state.Metadata()
	if !errors.Is(err, ErrDocRangeIndexPending) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
		test.Fatalf("unfinished work exposed metadata: valid=%t err=%v", metadata.Valid(), err)
	}
}

func legacyMetadataWorkFixture(test testing.TB, proof VerifiedSessionDoc) (*LegacyRangeMetadataPreparation, LegacyByteBackfill, *legacyMetadataWorkStream) {
	test.Helper()
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	source := &legacyMetadataWorkStream{legacyTestStream: &legacyTestStream{}, test: test}
	if err == nil {
		plan, err = plan.Spool(context.Background(), source)
	}
	if err != nil {
		test.Fatal(err)
	}
	source.reads = nil
	ctx, cancel := context.WithCancel(context.Background())
	state, err := NewLegacyRangeMetadataPreparation(ctx, proof, plan)
	cancel()
	if err != nil || state == nil || len(source.reads) != 0 {
		test.Fatalf("constructor scanned the spool or rejected a bound plan: %v", err)
	}
	key, _ := plan.Key()
	if state.Reference() != proof.DocumentRef() || state.PlanKey() != key || state.ObjectCount() != 0 {
		test.Fatal("constructor changed source identity or precomputed metadata objects")
	}
	legacyMetadataWorkPending(test, state)
	return state, plan, source
}

func legacyMetadataWorkFinish(test testing.TB, state *LegacyRangeMetadataPreparation, plan LegacyByteBackfill, source *legacyMetadataWorkStream, mutate, bounded bool) (VerifiedLegacyDerivedRangeMetadata, []DocRangeObject) {
	test.Helper()
	spec, err := plan.Spec()
	if err != nil {
		test.Fatal(err)
	}
	var saved []DocRangeObject
	for step := 0; step < 2*spec.Blocks+8; step++ {
		legacyMetadataWorkPending(test, state)
		before, count := len(source.reads), state.ObjectCount()
		if bounded {
			retained, err := state.RetainedBytes()
			if err != nil || retained <= 0 || retained >= ConversationManifestChunkBytes/2 {
				test.Fatalf("pending small-event work retained plaintext or a full event plan: %d, %v", retained, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done, err := state.Step(ctx, func(context.Context, DocRangeObject) error {
				test.Fatal("pre-canceled step reached its callback")
				return nil
			})
			after, retainedError := state.RetainedBytes()
			if done || !errors.Is(err, context.Canceled) || retainedError != nil || after != retained || len(source.reads) != before || state.ObjectCount() != count {
				test.Fatal("pre-canceled step changed pending work", err, retainedError)
			}
			legacyMetadataWorkPending(test, state)
		}
		ctx, cancel := context.WithCancel(context.Background())
		source.ctx = ctx
		active := true
		done, err := state.Step(ctx, func(saveCtx context.Context, object DocRangeObject) error {
			if !active || saveCtx != ctx || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangePageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("save reused a callback/context or emitted unauthenticated bytes")
			}
			legacyMetadataWorkPending(test, state)
			saved = append(saved, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
			if mutate {
				object.Bytes[0] ^= 1
				object.Hash = HashContent(object.Bytes)
			}
			return nil
		})
		active = false
		cancel()
		if err != nil {
			test.Fatal(err)
		}
		if bounded {
			if len(source.reads)-before > 1 {
				test.Fatalf("step %d reread sealed blocks or consumed multiple source blocks: %v", step, source.reads[before:])
			}
			for ordinal := before; ordinal < len(source.reads); ordinal++ {
				want := canonicalSpan{offset: ordinal * ConversationManifestChunkBytes, length: int(min(int64(ConversationManifestChunkBytes), spec.StreamBytes-int64(ordinal)*ConversationManifestChunkBytes))}
				if ordinal >= spec.Blocks || source.reads[ordinal] != want {
					test.Fatalf("source read %d = %+v, want exactly original block %+v", ordinal, source.reads[ordinal], want)
				}
			}
		}
		if done {
			metadata, err := state.Metadata()
			if err != nil || !metadata.Valid() || state.ObjectCount() != len(saved) || metadata.ObjectCount() != len(saved) || (bounded && len(source.reads) != spec.Blocks) {
				test.Fatal("completion skipped source blocks or failed to seal acknowledged objects", err)
			}
			if retained, err := state.RetainedBytes(); err != nil || retained <= 0 || retained >= ConversationManifestChunkBytes/2 {
				test.Fatalf("completed work retained event plaintext instead of compact metadata: %d, %v", retained, err)
			}
			return metadata, saved
		}
	}
	test.Fatal("metadata preparation did not finish")
	return VerifiedLegacyDerivedRangeMetadata{}, nil
}

func TestLegacyRangeMetadataPreparationParity(test *testing.T) {
	for _, fixture := range []struct {
		name        string
		count       int
		fingerprint ContentHash
	}{
		{"empty", 0, "sha256:1840b03939874ad97dc2582d30eca4e5093ef43ac0d7bee3ccb636f56a0217ce"},
		{"event-pages", DocRangePageEvents + 3, "sha256:1ce99541a5b530826077824e5b6c94e441e022d0c47266d461159c6a68273d17"},
		{"byte-blocks", 0, "sha256:2537c93cb7c0b4406bad846095e95e0ee62fb1be2a5068a6e5ee017eede1ffa6"},
		{"bounded-many-small-events", DocRangePageEvents*DocRangeDirectoryFanout + 1, "sha256:68a0569ce0495f9b4e028da66b54a62315fd379afa0a3852306683fa5c315d5b"},
		{"unicode", 0, ""}, {"escape", 0, ""}, {"compaction", 0, ""}, {"callback-mutation", DocRangePageEvents + 3, ""},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var proof VerifiedSessionDoc
			switch fixture.name {
			case "byte-blocks":
				proof, _ = legacyDocByteGenerationLargeFixture(test)
			case "unicode", "escape":
				marker := "한🙂"
				if fixture.name == "escape" {
					marker = "\\\"\n\t"
				}
				proof, _ = legacyDocByteBackfillBoundaryFixture(test, marker)
			case "compaction":
				proof, _ = legacyDocByteBackfillDocumentFixture(test, CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{
					{Kind: EventCompaction, Seq: 17, ReplacementComplete: true, Replacement: []CIREvent{
						{Kind: EventMessage, Seq: 9, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "한🙂 \\\" }],{"}}},
						{Kind: EventTurn, Seq: 3, Role: RoleAssistant},
					}},
				}})
			default:
				proof, _ = legacyDocRangeGenerationFixture(test, fixture.count)
			}
			var expected []DocRangeObject
			want, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
				expected = append(expected, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
				return nil
			})
			if err != nil {
				test.Fatal(err)
			}
			state, plan, source := legacyMetadataWorkFixture(test, proof)
			got, saved := legacyMetadataWorkFinish(test, state, plan, source, fixture.name == "callback-mutation", fixture.name == "bounded-many-small-events")
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(saved, expected) {
				test.Fatal("incremental metadata changed oracle descriptor, ordered hashes, sizes, or bytes")
			}
			fingerprint := struct {
				Descriptor json.RawMessage `json:"descriptor"`
				Objects    []ContentHash   `json:"objects"`
			}{Descriptor: got.descriptor, Objects: make([]ContentHash, 0, len(saved))}
			for _, object := range saved {
				fingerprint.Objects = append(fingerprint.Objects, object.Hash)
			}
			raw, err := json.Marshal(fingerprint)
			if err != nil || (fixture.fingerprint != "" && HashContent(raw) != fixture.fingerprint) {
				test.Fatal("incremental metadata changed frozen shared-page/directory fingerprint", HashContent(raw), err)
			}
		})
	}
}

func TestLegacyRangeMetadataPreparationFailuresPoisonState(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 4097)
	for _, fault := range []string{"read-cancel", "read-error", "save-cancel", "save-error", "spool-hash"} {
		test.Run(fault, func(test *testing.T) {
			state, plan, source := legacyMetadataWorkFixture(test, proof)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source.ctx = ctx
			done, err := state.Step(ctx, func(context.Context, DocRangeObject) error {
				legacyMetadataWorkPending(test, state)
				return nil
			})
			if err != nil || done || state.ObjectCount() == 0 {
				test.Fatal("fixture did not yield after saving partial metadata", done, err)
			}
			legacyMetadataWorkPending(test, state)
			original := bytes.Clone(source.body)
			failure := errors.New("metadata work failed")
			want := failure
			switch fault {
			case "read-cancel":
				source.readTransform = func(body []byte) []byte { cancel(); return body }
				want = context.Canceled
			case "read-error":
				source.readError = failure
			case "save-cancel":
				want = context.Canceled
			case "spool-hash":
				position := bytes.Index(source.body[ConversationManifestChunkBytes:], []byte("public-message-only"))
				if position < 0 {
					test.Fatal("missing same-length valid-JSON corruption target")
				}
				source.body[ConversationManifestChunkBytes+position] = 'P'
				want = ErrPreparedDocRangeObject
			}
			spec, _ := plan.Spec()
			for step := 0; step < spec.Blocks+8; step++ {
				done, err = state.Step(ctx, func(context.Context, DocRangeObject) error {
					legacyMetadataWorkPending(test, state)
					if fault == "save-error" {
						return failure
					}
					if fault == "save-cancel" {
						cancel()
					}
					return nil
				})
				if err != nil || done {
					break
				}
			}
			metadata, metadataError := state.Metadata()
			if done || !errors.Is(err, want) || !errors.Is(metadataError, ErrIntegrity) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
				test.Fatal("failed preparation exposed partial metadata or lost its failure", err, metadataError)
			}
			source.body, source.readError, source.readTransform = original, nil, nil
			source.ctx = context.Background()
			reads := len(source.reads)
			done, err = state.Step(source.ctx, func(context.Context, DocRangeObject) error {
				test.Fatal("poisoned state resumed callbacks")
				return nil
			})
			if done || err == nil || len(source.reads) != reads {
				test.Fatal("repairing the source revived poisoned work", err)
			}
		})
	}
}

func TestLegacyRangeMetadataPreparationPreservesExpandedVerifiedEvent(test *testing.T) {
	proof, _ := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
		Envelope: CIREnvelope{CIRVersion: "2"},
		Events: []CIREvent{{Kind: EventMessage, Role: RoleUser,
			Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("<", 6<<20)}}}},
	})
	var expected []DocRangeObject
	want, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
		expected = append(expected, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	})
	if err != nil {
		test.Fatal("existing metadata path rejected its verified expanded event", err)
	}
	state, plan, source := legacyMetadataWorkFixture(test, proof)
	spec, err := plan.Spec()
	if err != nil || spec.StreamBytes <= MaxLegacyStreamEventBytes {
		test.Fatal("canonical escaping did not exceed the separate streaming-admission limit", spec, err)
	}
	got, saved := legacyMetadataWorkFinish(test, state, plan, source, false, false)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(saved, expected) {
		test.Fatal("incremental metadata changed the previously accepted expanded event")
	}
}

func TestLegacyRangeMetadataPreparationRejectsUnboundInputs(test *testing.T) {
	proof, raw := legacyDocRangeGenerationFixture(test, 1)
	_, spooled, source := legacyMetadataWorkFixture(test, proof)
	unspooled, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	other, _ := legacyDocRangeGenerationFixture(test, 2)
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	rootProof, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	for name, candidate := range map[string]struct {
		proof VerifiedSessionDoc
		plan  LegacyByteBackfill
	}{
		"zero": {}, "zero-proof": {plan: spooled}, "zero-plan": {proof: proof},
		"unspooled": {proof, unspooled}, "mismatched": {other, spooled}, "root": {rootProof, spooled},
	} {
		test.Run(name, func(test *testing.T) {
			state, err := NewLegacyRangeMetadataPreparation(context.Background(), candidate.proof, candidate.plan)
			if err == nil || state != nil || len(source.reads) != 0 {
				test.Fatal("unbound constructor input acquired work or read the source", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if state, err := NewLegacyRangeMetadataPreparation(ctx, proof, spooled); !errors.Is(err, context.Canceled) || state != nil || len(source.reads) != 0 {
		test.Fatal("pre-canceled construction acquired work", err)
	}
}
