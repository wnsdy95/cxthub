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

type legacyRangeSinglePassStream struct {
	source    *legacyTestStream
	reads     []canonicalSpan
	readError error
}

func (stream *legacyRangeSinglePassStream) Append(ctx context.Context, body []byte) error {
	return stream.source.Append(ctx, body)
}

func (stream *legacyRangeSinglePassStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	stream.reads = append(stream.reads, canonicalSpan{offset: offset, length: length})
	if stream.readError != nil {
		return nil, stream.readError
	}
	return stream.source.Read(ctx, offset, length)
}

func legacyRangeSinglePassAssertCanonical(test testing.TB, proof VerifiedSessionDoc, generation DocRangeGeneration, objects []DocRangeObject) {
	test.Helper()
	var canonical struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(proof.Bytes(), &canonical); err != nil {
		test.Fatal(err)
	}
	if generation.Version != 3 || generation.Algorithm != 1 || generation.Hash != proof.Hash() ||
		generation.SourceIdentity == nil || *generation.SourceIdentity != DocumentIdentityLegacy ||
		generation.EventCount != int64(len(canonical.Events)) || !bytes.Equal(generation.Envelope, canonical.Envelope) ||
		generation.ChunkPages != (DocRangeDirectoryRoot{}) {
		test.Fatalf("generation changed canonical source metadata: %+v", generation)
	}
	entries := legacyDocRangeGenerationPages(test, generation, objects)
	offset, rendered := 0, 0
	for position, raw := range canonical.Events {
		var event CIREvent
		if err := json.Unmarshal(raw, &event); err != nil {
			test.Fatal(err)
		}
		empty := event.Kind == EventReasoning && strings.Trim(event.RedactedSummary, " \ufeff\t\n") == ""
		seed := event.Kind == EventMessage && len(event.Blocks) > 0 && strings.HasPrefix(strings.TrimLeft(event.Blocks[0].Text, " \ufeff"), "[cxt seed] Branch-switch context:")
		want := DocRangeEntry{
			DocEventIndex: DocEventIndex{Index: position, Offset: offset, Length: len(raw), Hash: HashContent(raw), Seq: event.Seq, Role: string(event.Role)},
			Kind:          event.Kind, ToolName: event.ToolName, CompactSummary: event.CompactSummary,
			SeedSummary: seed, EmptyReasoning: empty, RenderedBefore: rendered,
		}
		if entries[position] != want {
			test.Fatalf("canonical event %d changed: got %+v, want %+v", position, entries[position], want)
		}
		offset += len(raw) + 1
		if !empty {
			rendered++
		}
	}
	if generation.StreamBytes != int64(max(0, offset-1)) {
		test.Fatalf("stream bytes = %d, want %d", generation.StreamBytes, max(0, offset-1))
	}
}

func TestLegacyDocRangeSinglePassRawUnknownSequences(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents + 3, 2*DocRangePageEvents + 7} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, original := legacyDocRangeGenerationFixture(test, count)
			plan, err := proof.PlanReadIndexContext(context.Background())
			if err != nil || proof.chunks != nil || plan.sequencesKnown {
				test.Fatalf("fixture must be a raw monolithic plan without retained sequences: %v", err)
			}
			for _, location := range plan.index.Events {
				if location.Seq != 0 {
					test.Fatal("raw planning unexpectedly populated event sequences")
				}
			}
			generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			legacyRangeSinglePassAssertCanonical(test, proof, generation, objects)
			entries := legacyDocRangeGenerationPages(test, generation, objects)
			for position, entry := range entries {
				if want := 17 + 7*(position/2); entry.Seq != want {
					test.Fatalf("reversed input lost canonical gaps/equal sequences at %d: got %d, want %d", position, entry.Seq, want)
				}
			}
			if proof.DocumentRef() != (DocumentRef{Hash: HashContent(original), Identity: DocumentIdentityLegacy}) || !bytes.Equal(proof.Bytes(), original) {
				test.Fatal("raw generation changed its source proof")
			}
		})
	}
}

func TestLegacyDocRangeSinglePassSyntheticPrivatePlanReadsOnlyCurrentSource(test *testing.T) {
	materialized, original := legacyDocRangeGenerationFixture(test, DocRangePageEvents+3)
	streamed, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
	originalStream, originalManifest := bytes.Clone(spool.body), streamed.Manifest()
	baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), materialized)
	if err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"raw-unknown", "streamed-known", "streamed-synthetic-unknown"} {
		test.Run(name, func(test *testing.T) {
			var plan DocReadPlan
			var err error
			if name == "raw-unknown" {
				plan, err = materialized.PlanReadIndexContext(context.Background())
			} else {
				plan, err = streamed.PlanReadIndexContext(context.Background())
			}
			if err != nil {
				test.Fatal(err)
			}
			if name == "streamed-synthetic-unknown" {
				plan.sequencesKnown = false
				for position := range plan.index.Events {
					plan.index.Events[position].Seq = 0
				}
			}
			indexBefore := plan.index
			indexBefore.Envelope = plan.Envelope()
			indexBefore.Events = append([]DocEventIndex{}, plan.index.Events...)
			spansBefore := append([]canonicalSpan{}, plan.bodies...)
			planSource := &legacyRangeSinglePassStream{source: spool, readError: errors.New("synthetic metadata prepass is forbidden")}
			plan.source = planSource
			currentSource := &legacyRangeSinglePassStream{source: spool}
			generation, objects, err := buildLegacyDocRangeGeneration(context.Background(), materialized.DocumentRef(), plan, func(offset, length int) ([]byte, error) {
				return currentSource.Read(context.Background(), offset, length)
			})
			if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) {
				test.Fatalf("single current-byte pass changed descriptor or ordered object bytes: %v", err)
			}
			if len(planSource.reads) != 0 || !reflect.DeepEqual(currentSource.reads, spansBefore) {
				test.Fatalf("expected one explicit callback per planned event and no synthetic prepass: plan=%v current=%v want=%v", planSource.reads, currentSource.reads, spansBefore)
			}
			if !reflect.DeepEqual(plan.index, indexBefore) || !reflect.DeepEqual(plan.bodies, spansBefore) {
				test.Fatal("generation mutated the immutable event plan")
			}
			legacyRangeSinglePassAssertCanonical(test, materialized, generation, objects)
			if !bytes.Equal(spool.body, originalStream) || !bytes.Equal(materialized.Bytes(), original) ||
				streamed.Reference().DocumentRef() != materialized.DocumentRef() || !reflect.DeepEqual(streamed.Manifest(), originalManifest) {
				test.Fatal("generation changed its original proof or stream")
			}
		})
	}
}

func TestLegacyDocRangeSinglePassCurrentReadFailuresAreAtomic(test *testing.T) {
	materialized, original := legacyDocRangeGenerationFixture(test, DocRangePageEvents+3)
	streamed, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
	originalStream, originalManifest := bytes.Clone(spool.body), streamed.Manifest()
	baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), materialized)
	if err != nil {
		test.Fatal(err)
	}
	readFailure := errors.New("synthetic current event read failure")
	for _, representation := range []string{"raw-unknown", "streamed-known"} {
		test.Run(representation, func(test *testing.T) {
			var plan DocReadPlan
			var err error
			if representation == "raw-unknown" {
				plan, err = materialized.PlanReadIndexContext(context.Background())
			} else {
				plan, err = streamed.PlanReadIndexContext(context.Background())
			}
			if err != nil {
				test.Fatal(err)
			}
			indexBefore, err := json.Marshal(plan.index)
			if err != nil {
				test.Fatal(err)
			}
			for _, faultAt := range []int{0, len(plan.index.Events) - 1} {
				for _, fault := range []struct {
					name string
					want error
				}{
					{name: "changed-valid-json", want: ErrIntegrity},
					{name: "short-read", want: ErrIntegrity},
					{name: "long-read", want: ErrIntegrity},
					{name: "reader-error", want: readFailure},
					{name: "cancel-during-read", want: context.Canceled},
				} {
					test.Run(fmt.Sprintf("%s/event-%d", fault.name, faultAt), func(test *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						reads, faultActive := 0, true
						read := func(offset, length int) ([]byte, error) {
							position := reads
							reads++
							if position >= len(plan.index.Events) || offset != plan.index.Events[position].Offset || length != plan.index.Events[position].Length {
								test.Fatalf("unexpected current event read %d: offset=%d length=%d", position, offset, length)
							}
							body, err := spool.Read(context.Background(), offset, length)
							if err != nil || !faultActive || position != faultAt {
								return body, err
							}
							switch fault.name {
							case "changed-valid-json":
								changed := bytes.Clone(body)
								sequenceOffset := bytes.Index(changed, []byte(`"seq":`))
								if sequenceOffset < 0 {
									test.Fatal("fixture has no event sequence")
								}
								sequenceOffset += len(`"seq":`)
								changed[sequenceOffset] = '9'
								if bytes.Equal(changed, body) || len(changed) != len(body) || !json.Valid(changed) {
									test.Fatal("current-byte corruption must change only the hash, not JSON validity or length")
								}
								return changed, nil
							case "short-read":
								return body[:len(body)-1], nil
							case "long-read":
								return append(bytes.Clone(body), ' '), nil
							case "reader-error":
								return nil, readFailure
							case "cancel-during-read":
								cancel()
							}
							return body, nil
						}
						generation, objects, err := buildLegacyDocRangeGeneration(ctx, materialized.DocumentRef(), plan, read)
						if !errors.Is(err, fault.want) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 || reads != faultAt+1 {
							test.Fatalf("fault leaked partial output or read beyond failure: generation=%+v objects=%d reads=%d err=%v want=%v", generation, len(objects), reads, err, fault.want)
						}
						indexAfter, err := json.Marshal(plan.index)
						if err != nil || !bytes.Equal(indexAfter, indexBefore) || !bytes.Equal(spool.body, originalStream) ||
							!bytes.Equal(materialized.Bytes(), original) || streamed.Reference().DocumentRef() != materialized.DocumentRef() || !reflect.DeepEqual(streamed.Manifest(), originalManifest) {
							test.Fatalf("failed generation mutated its plan, proof, or source: %v", err)
						}
						reads, faultActive = 0, false
						rebuilt, rebuiltObjects, err := buildLegacyDocRangeGeneration(context.Background(), materialized.DocumentRef(), plan, read)
						if err != nil || reads != len(plan.index.Events) || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) {
							test.Fatalf("repaired reader failed to reproduce baseline bytes: reads=%d err=%v", reads, err)
						}
					})
				}
			}
		})
	}
}

func TestLegacyDocRangeSinglePassRejectsDiscontinuousCoordinates(test *testing.T) {
	proof, original := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	for _, faultAt := range []int{0, DocRangePageEvents} {
		test.Run(fmt.Sprint(faultAt), func(test *testing.T) {
			plan, err := proof.PlanReadIndexContext(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			originalLocations := append([]DocEventIndex{}, plan.index.Events...)
			plan.index.Events[faultAt].Offset++
			reads := 0
			generation, objects, err := buildLegacyDocRangeGeneration(context.Background(), proof.DocumentRef(), plan, func(offset, length int) ([]byte, error) {
				position := reads
				reads++
				if position >= len(originalLocations) || offset != plan.index.Events[position].Offset || length != originalLocations[position].Length {
					test.Fatalf("unexpected discontinuous event read %d: offset=%d length=%d", position, offset, length)
				}
				location := originalLocations[position]
				return proof.EventStreamRange(context.Background(), location.Offset, location.Length)
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 || reads > faultAt+1 {
				test.Fatalf("discontinuous coordinates produced partial output: generation=%+v objects=%d reads=%d err=%v", generation, len(objects), reads, err)
			}
			if !bytes.Equal(proof.Bytes(), original) {
				test.Fatal("coordinate rejection changed the original proof")
			}
		})
	}
}

func TestLegacyDocRangeSinglePassCancellationCheckpointsAreAtomic(test *testing.T) {
	for _, count := range []int{0, 3} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, original := legacyDocRangeGenerationFixture(test, count)
			plan, err := proof.PlanReadIndexContext(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			read := func(offset, length int) ([]byte, error) {
				return proof.EventStreamRange(context.Background(), offset, length)
			}
			checks := 0
			baseline, baselineObjects, err := buildLegacyDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof.DocumentRef(), plan, read)
			if err != nil || checks == 0 {
				test.Fatalf("missing private builder cancellation checkpoints: %d, %v", checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				ctx, cancel := context.WithCancel(context.Background())
				seen := 0
				generation, objects, err := buildLegacyDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
					seen++
					if seen == cancelAt {
						cancel()
					}
				}}, proof.DocumentRef(), plan, read)
				cancel()
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
					test.Fatalf("checkpoint %d leaked output after cancellation: generation=%+v objects=%d err=%v", cancelAt, generation, len(objects), err)
				}
			}
			rebuilt, rebuiltObjects, err := buildLegacyDocRangeGeneration(context.Background(), proof.DocumentRef(), plan, read)
			if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) || !bytes.Equal(proof.Bytes(), original) {
				test.Fatalf("cancellation changed the source or retry: %v", err)
			}
		})
	}
}

func TestLegacyDocRangeTerminalCancellationNeverReturnsGeneration(tests *testing.T) {
	proof, raw := legacyDocRangeGenerationFixture(tests, 3)
	streamed, _ := legacyDocRangeGenerationStreamFixture(tests, raw, 31)
	envelope, events := chunkVerifierParts(tests, raw)
	nonnormalized := chunkVerifierPlan(envelope, events, 31).Manifest
	for _, builder := range []struct {
		name  string
		build func(context.Context) (DocRangeGeneration, []DocRangeObject, error)
	}{
		{name: "raw-v3", build: func(ctx context.Context) (DocRangeGeneration, []DocRangeObject, error) {
			return BuildLegacyDocRangeGeneration(ctx, proof)
		}},
		{name: "stream-v3", build: func(ctx context.Context) (DocRangeGeneration, []DocRangeObject, error) {
			return BuildStreamedLegacyDocRangeGeneration(ctx, streamed)
		}},
		{name: "stream-v4", build: func(ctx context.Context) (DocRangeGeneration, []DocRangeObject, error) {
			return BuildStreamedLegacyDocByteRangeGeneration(ctx, streamed, streamed.Manifest())
		}},
		{name: "stream-v3-compatibility", build: func(ctx context.Context) (DocRangeGeneration, []DocRangeObject, error) {
			return BuildStreamedLegacyDocByteRangeGeneration(ctx, streamed, nonnormalized)
		}},
		{name: "derived-v5", build: func(ctx context.Context) (DocRangeGeneration, []DocRangeObject, error) {
			return BuildLegacyDerivedDocByteRangeGeneration(ctx, proof)
		}},
	} {
		tests.Run(builder.name, func(test *testing.T) {
			checks := 0
			if _, _, err := builder.build(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}); err != nil || checks == 0 {
				test.Fatal("could not establish terminal cancellation checkpoint", checks, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := 0
			generation, objects, err := builder.build(canonicalAdmissionContext{Context: ctx, check: func() {
				observed++
				if observed == checks {
					cancel()
				}
			}})
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatal("terminal cancellation exposed a reusable generation", observed, checks, generation, len(objects), err)
			}
		})
	}
}

func TestLegacyDocRangeSinglePassRejectsInvalidPlanReferenceAndReader(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 1)
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name      string
		plan      DocReadPlan
		reference DocumentRef
		nilReader bool
	}{
		{name: "zero-plan", reference: proof.DocumentRef()},
		{name: "zero-reference", plan: plan},
		{name: "zero-plan-and-reference"},
		{name: "different-hash", plan: plan, reference: DocumentRef{Hash: HashContent([]byte("another source")), Identity: DocumentIdentityLegacy}},
		{name: "malformed-hash", plan: plan, reference: DocumentRef{Hash: "invalid", Identity: DocumentIdentityLegacy}},
		{name: "root-identity", plan: plan, reference: DocumentRef{Hash: proof.Hash(), Identity: DocumentIdentityRootV1}},
		{name: "unknown-identity", plan: plan, reference: DocumentRef{Hash: proof.Hash(), Identity: "future"}},
		{name: "nil-required-reader", plan: plan, reference: proof.DocumentRef(), nilReader: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			read := func(offset, length int) ([]byte, error) {
				test.Fatal("invalid plan or reference reached the current-byte reader")
				return nil, ErrIntegrity
			}
			if fixture.nilReader {
				read = nil
			}
			generation, objects, err := buildLegacyDocRangeGeneration(context.Background(), fixture.reference, fixture.plan, read)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("invalid private builder input produced output: generation=%+v objects=%d err=%v", generation, len(objects), err)
			}
		})
	}
}
