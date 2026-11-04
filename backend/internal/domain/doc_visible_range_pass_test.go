package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func docVisibleRangePassPredecessor(ctx context.Context, proof VerifiedDocRangeGeneration, emit func(context.Context, DocRangeObject) error, processedPage func(int)) (DocVisibleGeneration, error) {
	if err := ctx.Err(); err != nil {
		return DocVisibleGeneration{}, err
	}
	verified, ranges := proof.index, proof.hash
	if emit == nil || ValidateContentHash(ranges) != nil || verified.read == nil || verified.presentationAlgorithm != DocVisibleAlgorithm || verified.index.Validate(verified.index.Hash) != nil {
		return DocVisibleGeneration{}, ErrIntegrity
	}
	generation := DocVisibleGeneration{Version: 1, Algorithm: DocVisibleAlgorithm, Hash: verified.index.Hash, RangeGeneration: ranges, Events: int(verified.index.Manifest.EventCount), Views: make(map[string]DocVisibleRoot, 3)}
	builders := make([]docVisibleBuilder, 0, 3)
	for _, mode := range []string{"all", "chat", "prompts"} {
		builders = append(builders, docVisibleBuilder{ctx: ctx, mode: mode, total: generation.Events, emit: emit})
	}
	turn, next := -1, 0
	for pageOrdinal := range verified.index.Pages {
		_, entries, err := verified.pageEntries(ctx, pageOrdinal)
		if err != nil {
			return DocVisibleGeneration{}, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return DocVisibleGeneration{}, err
			}
			if entry.Index != next || entry.RenderedBefore != generation.Rendered {
				return DocVisibleGeneration{}, ErrIntegrity
			}
			prompt := entry.Kind == EventMessage && entry.Role == string(RoleUser) && !entry.CompactSummary && !entry.SeedSummary
			if prompt {
				turn = entry.Index
			}
			for builderPosition := range builders {
				if err := builders[builderPosition].add(entry, turn, prompt); err != nil {
					return DocVisibleGeneration{}, err
				}
			}
			if !entry.EmptyReasoning {
				generation.Rendered++
			}
			next++
		}
		if processedPage != nil {
			processedPage(pageOrdinal)
		}
	}
	if next != generation.Events {
		return DocVisibleGeneration{}, ErrIntegrity
	}
	for builderPosition := range builders {
		root, err := builders[builderPosition].finish()
		if err != nil {
			return DocVisibleGeneration{}, err
		}
		generation.Views[builders[builderPosition].mode] = root
	}
	if err := generation.Validate(generation.Hash, ranges); err != nil {
		return DocVisibleGeneration{}, err
	}
	if err := ctx.Err(); err != nil {
		return DocVisibleGeneration{}, err
	}
	return generation, nil
}

func docVisibleRangePassEvents(count int) []CIREvent {
	pattern := docVisibleRendererEvents()
	compact := docVisibleTestMessage(RoleUser, "compact summary")
	compact.CompactSummary = true
	pattern = append(pattern, compact, docVisibleTestMessage(RoleUser, " \ufeff[cxt] This session was resumed from a branch context seed. inherited"))
	events := make([]CIREvent, count)
	for position := range events {
		events[position] = pattern[position%len(pattern)]
	}
	return events
}

func docVisibleRangePassPages(test *testing.T, proof VerifiedDocRangeGeneration) []DocRangeObject {
	test.Helper()
	pages := make([]DocRangeObject, len(proof.index.index.Pages))
	for ordinal, hash := range proof.index.index.Pages {
		pages[ordinal] = DocRangeObject{Hash: hash, Bytes: docRangeTestPage(test, proof.index, ordinal)}
	}
	return pages
}

func docVisibleRangePassEmitter(test *testing.T, objects *[]DocRangeObject) func(context.Context, DocRangeObject) error {
	test.Helper()
	return func(ctx context.Context, object DocRangeObject) error {
		test.Helper()
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocVisibleObjectBytes || HashContent(object.Bytes) != object.Hash {
			test.Fatalf("invalid visible object: %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		*objects = append(*objects, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return ctx.Err()
	}
}

func docVisibleRangePassAssertOutput(test *testing.T, generation, expected DocVisibleGeneration, objects, expectedObjects []DocRangeObject) {
	test.Helper()
	if !reflect.DeepEqual(generation, expected) || !reflect.DeepEqual(objects, expectedObjects) {
		test.Fatal("descriptor or ordered visible object hashes/bytes differ from predecessor")
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	expectedRaw, err := expected.Encode()
	if err != nil || !bytes.Equal(raw, expectedRaw) {
		test.Fatalf("encoded visible descriptor differs: %v", err)
	}
}

func docVisibleRangePassAssertError(test *testing.T, generation DocVisibleGeneration, err, expected error) {
	test.Helper()
	if !errors.Is(err, expected) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
		test.Fatalf("failed build returned %+v, %v; want zero generation and %v", generation, err, expected)
	}
}

func TestDocVisibleRangePassPredecessorParity(test *testing.T) {
	var canonical CIRDocument
	decoder := json.NewDecoder(bytes.NewReader(verifierFixture(test)))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		test.Fatal(err)
	}
	cases := []struct {
		name   string
		events []CIREvent
	}{
		{name: "canonical-verifier-fixture", events: canonical.Events},
		{name: "renderer-fixture", events: docVisibleRendererEvents()},
	}
	for _, count := range []int{0, 1, DocRangePageEvents - 1, DocRangePageEvents, DocRangePageEvents + 1, 2*DocRangePageEvents + 3} {
		cases = append(cases, struct {
			name   string
			events []CIREvent
		}{name: fmt.Sprintf("mixed-%d", count), events: docVisibleRangePassEvents(count)})
	}
	prompts := make([]CIREvent, 2*DocRangePageEvents+1)
	fold := make([]CIREvent, len(prompts))
	hidden := make([]CIREvent, DocRangePageEvents+1)
	for position := range prompts {
		prompts[position] = docVisibleTestMessage(RoleUser, "prompt")
		fold[position] = CIREvent{Kind: EventToolResult, Output: "work"}
	}
	fold[0], fold[len(fold)-1] = docVisibleTestMessage(RoleUser, "prompt"), docVisibleTestMessage(RoleAssistant, "answer")
	for position := range hidden {
		hidden[position] = CIREvent{Kind: EventReasoning}
	}
	cases = append(cases, struct {
		name   string
		events []CIREvent
	}{"all-view-page-boundaries", prompts}, struct {
		name   string
		events []CIREvent
	}{"cross-page-fold", fold}, struct {
		name   string
		events []CIREvent
	}{"only-empty-reasoning", hidden})
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := docVisibleTestFixture(test, scenario.events)
			sourceReads := *fixture.sourceReads
			for _, representation := range []struct {
				name  string
				index VerifiedDocRangeIndex
			}{{"streamed", fixture.streamed}, {"materialized", fixture.materialized}} {
				test.Run(representation.name, func(test *testing.T) {
					proof := docVisibleTestRangeProof(test, representation.index)
					pages := docVisibleRangePassPages(test, proof)
					var expectedObjects []DocRangeObject
					var expectedTrace []string
					recordExpected := docVisibleRangePassEmitter(test, &expectedObjects)
					expected, err := docVisibleRangePassPredecessor(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
						expectedTrace = append(expectedTrace, "visible:"+string(object.Hash))
						return recordExpected(ctx, object)
					}, func(ordinal int) {
						expectedTrace = append(expectedTrace, fmt.Sprintf("range:%d", ordinal))
					})
					if err != nil {
						test.Fatal(err)
					}
					for _, withRangePages := range []bool{false, true} {
						var objects []DocRangeObject
						var trace []string
						reads := make([]int, len(pages))
						rangeCalls := 0
						observed := proof
						observed.index.read = func(ctx context.Context, ordinal int) ([]byte, error) {
							if ordinal < 0 || ordinal >= len(reads) || reads[ordinal] != 0 || withRangePages && ordinal != rangeCalls {
								test.Fatalf("unexpected or premature page read %d: reads=%v ranges=%d", ordinal, reads, rangeCalls)
							}
							reads[ordinal]++
							return proof.index.read(ctx, ordinal)
						}
						record := docVisibleRangePassEmitter(test, &objects)
						emit := func(ctx context.Context, object DocRangeObject) error {
							trace = append(trace, "visible:"+string(object.Hash))
							return record(ctx, object)
						}
						var generation DocVisibleGeneration
						if withRangePages {
							generation, err = BuildDocVisibleGenerationWithRangePages(context.Background(), observed, emit, func(ctx context.Context, ordinal int, object DocRangeObject) error {
								if ordinal != rangeCalls || ordinal >= len(pages) || object.Hash != pages[ordinal].Hash || !bytes.Equal(object.Bytes, pages[ordinal].Bytes) {
									test.Fatalf("range emission %d changed original ordinal, hash, or bytes", ordinal)
								}
								trace = append(trace, fmt.Sprintf("range:%d", ordinal))
								rangeCalls++
								return ctx.Err()
							})
							if rangeCalls != len(pages) || !reflect.DeepEqual(trace, expectedTrace) {
								test.Fatalf("range callbacks did not follow each processed page: calls=%d trace=%v want=%v", rangeCalls, trace, expectedTrace)
							}
						} else {
							generation, err = BuildDocVisibleGeneration(context.Background(), observed, emit)
						}
						if err != nil {
							test.Fatal(err)
						}
						for ordinal, count := range reads {
							if count != 1 {
								test.Fatalf("page %d read %d times; want exactly once", ordinal, count)
							}
						}
						docVisibleRangePassAssertOutput(test, generation, expected, objects, expectedObjects)
						objectMap := make(map[ContentHash][]byte, len(objects))
						for _, object := range objects {
							objectMap[object.Hash] = object.Bytes
						}
						for mode, view := range generation.Views {
							if view.Units == 0 {
								continue
							}
							for _, position := range []int{0, view.Units - 1} {
								_, ordinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, position, false, func(_ context.Context, hash ContentHash) ([]byte, error) {
									return objectMap[hash], nil
								})
								if err != nil || ordinal != position {
									test.Fatal("emitted visible boundary is unreadable", mode, position, ordinal, err)
								}
							}
						}
					}
				})
			}
			if *fixture.sourceReads != sourceReads {
				test.Fatal("visible generation reread canonical event bodies")
			}
		})
	}
}

func TestDocVisibleRangePassOwnsEmittedBytes(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRangePassEvents(2*DocRangePageEvents+3))
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	var expectedObjects []DocRangeObject
	expected, err := docVisibleRangePassPredecessor(context.Background(), proof, docVisibleRangePassEmitter(test, &expectedObjects), nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, reuseReaderBuffer := range []bool{false, true} {
		for _, mutateInCallback := range []bool{false, true} {
			test.Run(fmt.Sprintf("reused-reader=%t/mutate-in-callback=%t", reuseReaderBuffer, mutateInCallback), func(test *testing.T) {
				pages := docVisibleRangePassPages(test, proof)
				snapshots := docVisibleRangePassPages(test, proof)
				observed := proof
				buffer := make([]byte, 0, MaxDocRangePageBytes)
				observed.index.read = func(ctx context.Context, ordinal int) ([]byte, error) {
					if reuseReaderBuffer {
						buffer = append(buffer[:0], pages[ordinal].Bytes...)
						return buffer, ctx.Err()
					}
					return pages[ordinal].Bytes, ctx.Err()
				}
				var retainedVisible, retainedRanges, objects []DocRangeObject
				record := docVisibleRangePassEmitter(test, &objects)
				generation, err := BuildDocVisibleGenerationWithRangePages(context.Background(), observed, func(ctx context.Context, object DocRangeObject) error {
					if err := record(ctx, object); err != nil {
						return err
					}
					retainedVisible = append(retainedVisible, object)
					if mutateInCallback {
						clear(object.Bytes)
					}
					return nil
				}, func(ctx context.Context, ordinal int, object DocRangeObject) error {
					if ordinal != len(retainedRanges) || ordinal >= len(snapshots) || object.Hash != snapshots[ordinal].Hash || !bytes.Equal(object.Bytes, snapshots[ordinal].Bytes) {
						test.Fatal("range bytes were not original at emission")
					}
					retainedRanges = append(retainedRanges, object)
					if mutateInCallback {
						clear(object.Bytes)
					}
					return ctx.Err()
				})
				if err != nil || len(retainedRanges) != len(pages) {
					test.Fatalf("owned-byte build: %d range pages, %v", len(retainedRanges), err)
				}
				docVisibleRangePassAssertOutput(test, generation, expected, objects, expectedObjects)
				if !mutateInCallback && (!reflect.DeepEqual(retainedRanges, snapshots) || !reflect.DeepEqual(retainedVisible, expectedObjects)) {
					test.Fatal("retained emitted bytes were overwritten by later reads or emissions")
				}
				for _, object := range append(retainedRanges, retainedVisible...) {
					clear(object.Bytes)
				}
				if !reflect.DeepEqual(pages, snapshots) || !reflect.DeepEqual(observed.Descriptor(), proof.Descriptor()) || !reflect.DeepEqual(observed.index.Header(), proof.index.Header()) {
					test.Fatal("emitter mutation changed sealed source bytes or metadata")
				}
				var rebuiltObjects []DocRangeObject
				rebuilt, err := docVisibleRangePassPredecessor(context.Background(), observed, docVisibleRangePassEmitter(test, &rebuiltObjects), nil)
				if err != nil {
					test.Fatal("retained emitter mutation damaged source", err)
				}
				docVisibleRangePassAssertOutput(test, rebuilt, expected, rebuiltObjects, expectedObjects)
			})
		}
	}
}

func TestDocVisibleRangePassRejectsLaterPageFailures(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRangePassEvents(3*DocRangePageEvents+1))
	base := docVisibleTestRangeProof(test, fixture.streamed)
	readFailure := errors.New("later range page read failed")
	for _, failure := range []string{"read", "read-cancel", "hash", "json", "entry-order", "entry-count", "rendered-within-page", "rendered-cross-page-low", "rendered-cross-page-high"} {
		test.Run(failure, func(test *testing.T) {
			pages := docVisibleRangePassPages(test, base)
			verified := base.index
			verified.index = base.index.Header()
			raw := bytes.Clone(pages[1].Bytes)
			entries, err := verified.index.DecodePage(1, raw)
			if err != nil {
				test.Fatal(err)
			}
			expectedError := ErrIntegrity
			switch failure {
			case "read":
				expectedError = readFailure
			case "read-cancel":
				expectedError = context.Canceled
			case "hash":
				raw[0] = 'x'
			case "json":
				raw = []byte("{")
			case "entry-order":
				entries[1].Index = entries[0].Index
			case "entry-count":
				entries = entries[:len(entries)-1]
			case "rendered-within-page":
				entries[len(entries)-1].RenderedBefore--
			case "rendered-cross-page-low", "rendered-cross-page-high":
				for position := range entries {
					if failure == "rendered-cross-page-low" {
						entries[position].RenderedBefore--
					} else {
						entries[position].RenderedBefore++
					}
				}
			}
			switch failure {
			case "entry-order", "entry-count", "rendered-within-page", "rendered-cross-page-low", "rendered-cross-page-high":
				raw = docVisibleTestJSON(test, entries)
			}
			if failure != "hash" {
				verified.index.Pages[1] = HashContent(raw)
			}
			if failure == "rendered-within-page" || failure == "rendered-cross-page-low" || failure == "rendered-cross-page-high" {
				if _, err := verified.index.DecodePage(1, raw); err != nil {
					test.Fatal("rendered-count fixture must pass independent page validation", err)
				}
			}
			proof := docVisibleTestRangeProof(test, verified)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reads, emittedRanges []int
			proof.index.read = func(ctx context.Context, ordinal int) ([]byte, error) {
				reads = append(reads, ordinal)
				if ordinal == 1 {
					if failure == "read" {
						return nil, readFailure
					}
					if failure == "read-cancel" {
						cancel()
					}
					return raw, nil
				}
				return pages[ordinal].Bytes, ctx.Err()
			}
			generation, err := BuildDocVisibleGenerationWithRangePages(ctx, proof, func(context.Context, DocRangeObject) error { return nil }, func(ctx context.Context, ordinal int, object DocRangeObject) error {
				emittedRanges = append(emittedRanges, ordinal)
				if ordinal != 0 || object.Hash != pages[0].Hash || !bytes.Equal(object.Bytes, pages[0].Bytes) {
					test.Fatal("unprocessed or corrupted range page reached sink")
				}
				return ctx.Err()
			})
			docVisibleRangePassAssertError(test, generation, err, expectedError)
			if !reflect.DeepEqual(reads, []int{0, 1}) || !reflect.DeepEqual(emittedRanges, []int{0}) {
				test.Fatalf("later-page failure continued: reads=%v ranges=%v", reads, emittedRanges)
			}
		})
	}
}

func TestDocVisibleRangePassSinkFailuresStopImmediately(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRangePassEvents(2*DocRangePageEvents+3))
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	var expectedObjects []DocRangeObject
	var pageEmissionCounts []int
	_, err := docVisibleRangePassPredecessor(context.Background(), proof, docVisibleRangePassEmitter(test, &expectedObjects), func(int) {
		pageEmissionCounts = append(pageEmissionCounts, len(expectedObjects))
	})
	if err != nil {
		test.Fatal(err)
	}
	for _, sink := range []string{"visible", "range"} {
		failureCount := len(expectedObjects)
		if sink == "range" {
			failureCount = len(pageEmissionCounts)
		}
		for _, cancelInCallback := range []bool{false, true} {
			for failAt := 1; failAt <= failureCount; failAt++ {
				test.Run(fmt.Sprintf("%s-%d/cancel=%t", sink, failAt, cancelInCallback), func(test *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					failure := errors.New("visible/range sink failed")
					expectedError := failure
					if cancelInCallback {
						expectedError = context.Canceled
					}
					stop := func() error {
						if cancelInCallback {
							cancel()
							return nil
						}
						return failure
					}
					observed := proof
					var reads []int
					observed.index.read = func(ctx context.Context, ordinal int) ([]byte, error) {
						reads = append(reads, ordinal)
						return proof.index.read(ctx, ordinal)
					}
					var objects []DocRangeObject
					rangeCalls := 0
					record := docVisibleRangePassEmitter(test, &objects)
					generation, err := BuildDocVisibleGenerationWithRangePages(ctx, observed, func(emissionContext context.Context, object DocRangeObject) error {
						if emissionContext != ctx {
							test.Fatal("visible callback lost build context")
						}
						if err := record(emissionContext, object); err != nil {
							return err
						}
						if sink == "visible" && len(objects) == failAt {
							return stop()
						}
						return nil
					}, func(emissionContext context.Context, ordinal int, object DocRangeObject) error {
						if emissionContext != ctx || ordinal != rangeCalls || object.Hash != proof.index.index.Pages[ordinal] || HashContent(object.Bytes) != object.Hash {
							test.Fatal("range callback changed context, ordinal, or original object")
						}
						rangeCalls++
						if sink == "range" && rangeCalls == failAt {
							return stop()
						}
						return nil
					})
					docVisibleRangePassAssertError(test, generation, err, expectedError)
					expectedRanges, expectedVisible := 0, failAt
					if sink == "range" {
						expectedRanges, expectedVisible = failAt, pageEmissionCounts[failAt-1]
					} else {
						for _, count := range pageEmissionCounts {
							if count < failAt {
								expectedRanges++
							}
						}
					}
					expectedReads := min(expectedRanges+1, len(pageEmissionCounts))
					if sink == "range" {
						expectedReads = failAt
					}
					if rangeCalls != expectedRanges || len(objects) != expectedVisible || len(reads) != expectedReads || !reflect.DeepEqual(objects, expectedObjects[:expectedVisible]) {
						test.Fatalf("sink failure continued: reads=%v ranges=%d visible=%d; want %d/%d/%d", reads, rangeCalls, len(objects), expectedReads, expectedRanges, expectedVisible)
					}
					for ordinal, read := range reads {
						if read != ordinal {
							test.Fatalf("failure path reread or reordered pages: %v", reads)
						}
					}
				})
			}
		}
	}
}

func TestDocVisibleRangePassRejectsInvalidArgumentsBeforeReading(test *testing.T) {
	for _, count := range []int{0, 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := docVisibleTestFixture(test, docVisibleRangePassEvents(count))
			proof := docVisibleTestRangeProof(test, fixture.streamed)
			badHash, missingReader, legacy, future, badHeader := proof, proof, proof, proof, proof
			badHash.hash = "bad"
			missingReader.index.read = nil
			legacy.index.presentationAlgorithm = 0
			future.index.presentationAlgorithm = DocVisibleAlgorithm + 1
			badHeader.index.index = proof.index.Header()
			badHeader.index.index.Version = 0
			var decoded VerifiedDocRangeGeneration
			if err := json.Unmarshal(docVisibleTestJSON(test, proof.Descriptor()), &decoded); err != nil {
				test.Fatal(err)
			}
			for _, scenario := range []struct {
				name       string
				proof      VerifiedDocRangeGeneration
				nilVisible bool
				nilRange   bool
				canceled   bool
			}{
				{name: "nil-visible-sink", proof: proof, nilVisible: true},
				{name: "nil-range-sink", proof: proof, nilRange: true},
				{name: "both-nil", proof: proof, nilVisible: true, nilRange: true},
				{name: "zero-proof"},
				{name: "decoded-descriptor", proof: decoded},
				{name: "index-only", proof: VerifiedDocRangeGeneration{index: proof.index}},
				{name: "descriptor-and-hash-only", proof: VerifiedDocRangeGeneration{generation: proof.Descriptor(), hash: proof.hash}},
				{name: "unsealed-index-and-descriptor", proof: VerifiedDocRangeGeneration{index: proof.index, generation: proof.Descriptor()}},
				{name: "invalid-hash", proof: badHash},
				{name: "missing-reader", proof: missingReader},
				{name: "missing-presentation", proof: legacy},
				{name: "future-presentation", proof: future},
				{name: "invalid-header", proof: badHeader},
				{name: "canceled", proof: proof, canceled: true},
			} {
				test.Run(scenario.name, func(test *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					expectedError := ErrIntegrity
					if scenario.canceled {
						cancel()
						expectedError = context.Canceled
					}
					reads, emissions := 0, 0
					observed := scenario.proof
					if reader := observed.index.read; reader != nil {
						observed.index.read = func(ctx context.Context, ordinal int) ([]byte, error) {
							reads++
							return reader(ctx, ordinal)
						}
					}
					var emit func(context.Context, DocRangeObject) error
					if !scenario.nilVisible {
						emit = func(context.Context, DocRangeObject) error { emissions++; return nil }
					}
					var emitRange func(context.Context, int, DocRangeObject) error
					if !scenario.nilRange {
						emitRange = func(context.Context, int, DocRangeObject) error { emissions++; return nil }
					}
					generation, err := BuildDocVisibleGenerationWithRangePages(ctx, observed, emit, emitRange)
					docVisibleRangePassAssertError(test, generation, err, expectedError)
					if reads != 0 || emissions != 0 {
						test.Fatalf("invalid input caused %d page reads and %d emissions", reads, emissions)
					}
				})
			}
		})
	}
}
