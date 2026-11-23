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

type docVisibleFixture struct {
	events       []CIREvent
	streamed     VerifiedDocRangeIndex
	materialized VerifiedDocRangeIndex
	entries      []DocRangeEntry
	sourceReads  *int
	sourceHashes map[ContentHash][]byte
}

func docVisibleTestFixture(test *testing.T, events []CIREvent) docVisibleFixture {
	test.Helper()
	events = append([]CIREvent(nil), events...)
	for position := range events {
		events[position].Seq = position
	}
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: events})
	if err != nil {
		test.Fatal(err)
	}
	sourceReads := 0
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		sourceReads++
		return manifestProofLoad(bodies)(ctx, hash)
	}
	var verifier CanonicalDocVerifier
	hash := manifestProofHash(test, manifest)
	streamed, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, load, &docRangeMemoryBuffer{})
	if err != nil {
		test.Fatal(err)
	}
	document, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, load)
	if err != nil {
		test.Fatal(err)
	}
	materialized, err := BuildDocRangeIndex(context.Background(), document)
	if err != nil {
		test.Fatal(err)
	}
	header := streamed.Header()
	entries := make([]DocRangeEntry, 0, len(events))
	for page := range header.Pages {
		raw := docRangeTestPage(test, streamed, page)
		if !bytes.Equal(raw, docRangeTestPage(test, materialized, page)) {
			test.Fatalf("streamed and materialized metadata differ on page %d", page)
		}
		decoded, err := header.DecodePage(page, raw)
		if err != nil {
			test.Fatal(err)
		}
		entries = append(entries, decoded...)
	}
	return docVisibleFixture{events: events, streamed: streamed, materialized: materialized, entries: entries, sourceReads: &sourceReads, sourceHashes: bodies}
}

func docVisibleTestJSON(test testing.TB, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docVisibleTestRangeProof(test *testing.T, verified VerifiedDocRangeIndex) VerifiedDocRangeGeneration {
	test.Helper()
	proof, _, err := BuildVerifiedDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	return proof
}

func docVisibleTestBuild(test *testing.T, verified VerifiedDocRangeIndex) (DocVisibleGeneration, map[ContentHash][]byte) {
	test.Helper()
	proof := docVisibleTestRangeProof(test, verified)
	raw, err := proof.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
		if HashContent(object.Bytes) != object.Hash || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocVisibleObjectBytes {
			test.Fatalf("invalid emitted visible object: %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		if previous, exists := objects[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("same visible object hash has different bytes")
		}
		objects[object.Hash] = bytes.Clone(object.Bytes)
		return ctx.Err()
	})
	if err != nil {
		test.Fatal(err)
	}
	if err := generation.Validate(verified.Header().Hash, HashContent(raw)); err != nil {
		test.Fatal(err)
	}
	if err != nil {
		test.Fatal(err)
	}
	return generation, objects
}

func docVisibleTestMessage(role Role, text string) CIREvent {
	return CIREvent{Kind: EventMessage, Role: role, Blocks: []ContentBlock{{Type: "text", Text: text}}}
}

func docVisibleRendererEvents() []CIREvent {
	return []CIREvent{
		{Kind: EventToolResult, Output: "leading tool"},
		{Kind: EventCompaction, Replacement: []CIREvent{}, ReplacementComplete: true},
		docVisibleTestMessage(RoleUser, "[cxt seed] Branch-switch context: inherited"),
		docVisibleTestMessage(RoleUser, "first prompt"),
		{Kind: EventToolResult, Output: "work"},
		{Kind: EventReasoning},
		docVisibleTestMessage(RoleAssistant, "first answer"),
		{Kind: EventToolResult, Output: "more work"},
		docVisibleTestMessage(RoleAssistant, "second answer"),
		{Kind: EventToolResult, Output: "unfinished work"},
		docVisibleTestMessage(RoleUser, "second prompt"),
		{Kind: EventReasoning, RedactedSummary: "visible reasoning"},
	}
}

func TestDocVisibleGenerationStreamingEmissionOwnsObjectBytes(test *testing.T) {
	events := make([]CIREvent, 2*DocVisiblePageUnits+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	fixture := docVisibleTestFixture(test, events)
	want, wantObjects := docVisibleTestBuild(test, fixture.streamed)
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	for _, mutateInCallback := range []bool{false, true} {
		test.Run(fmt.Sprintf("mutate-in-callback=%t", mutateInCallback), func(test *testing.T) {
			var retained []DocRangeObject
			var snapshots [][]byte
			got, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatalf("discarding emitted bytes changed generation: %+v, %v", got, err)
			}
			got, err = BuildDocVisibleGeneration(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
				retained = append(retained, object)
				snapshots = append(snapshots, bytes.Clone(object.Bytes))
				if mutateInCallback {
					for position := range object.Bytes {
						object.Bytes[position] = 'x'
					}
				}
				return nil
			})
			if err != nil || !reflect.DeepEqual(got, want) || len(retained) < 6 {
				test.Fatalf("retained callback buffers changed generation: %d objects, %+v, %v", len(retained), got, err)
			}
			for position, object := range retained {
				if !bytes.Equal(snapshots[position], wantObjects[object.Hash]) || HashContent(snapshots[position]) != object.Hash {
					test.Fatalf("emission %d differs from independent build", position)
				}
				if !mutateInCallback && !bytes.Equal(object.Bytes, snapshots[position]) {
					test.Fatalf("emission %d aliases a later page/directory buffer", position)
				}
			}
			for _, object := range retained {
				object.Bytes[0] = 'x'
			}
			rebuilt, rebuiltObjects := docVisibleTestBuild(test, fixture.streamed)
			if !reflect.DeepEqual(rebuilt, want) || !reflect.DeepEqual(rebuiltObjects, wantObjects) {
				test.Fatal("caller-owned emitted bytes changed verified metadata or later builds")
			}
		})
	}
}

func docVisibleTestClone(generation DocVisibleGeneration) DocVisibleGeneration {
	views := make(map[string]DocVisibleRoot, len(generation.Views))
	for mode, root := range generation.Views {
		views[mode] = root
	}
	generation.Views = views
	return generation
}

func TestDocVisibleGenerationRejectsMalformedDescriptors(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}})
	generation, _ := docVisibleTestBuild(test, fixture.streamed)
	mutations := map[string]func(*DocVisibleGeneration){
		"version":               func(value *DocVisibleGeneration) { value.Version++ },
		"algorithm":             func(value *DocVisibleGeneration) { value.Algorithm++ },
		"document-binding":      func(value *DocVisibleGeneration) { value.Hash = HashContent([]byte("other document")) },
		"range-binding":         func(value *DocVisibleGeneration) { value.RangeGeneration = HashContent([]byte("other ranges")) },
		"invalid-document-hash": func(value *DocVisibleGeneration) { value.Hash = "bad" },
		"invalid-range-hash":    func(value *DocVisibleGeneration) { value.RangeGeneration = "bad" },
		"negative-events":       func(value *DocVisibleGeneration) { value.Events = -1 },
		"too-many-events":       func(value *DocVisibleGeneration) { value.Events = MaxConversationDocumentBytes + 1 },
		"negative-rendered":     func(value *DocVisibleGeneration) { value.Rendered = -1 },
		"too-many-rendered":     func(value *DocVisibleGeneration) { value.Rendered = value.Events + 1 },
		"missing-view":          func(value *DocVisibleGeneration) { delete(value.Views, "chat") },
		"unknown-view":          func(value *DocVisibleGeneration) { value.Views["unknown"] = value.Views["chat"] },
		"substituted-view": func(value *DocVisibleGeneration) {
			value.Views["unknown"] = value.Views["chat"]
			delete(value.Views, "chat")
		},
		"nil-views":           func(value *DocVisibleGeneration) { value.Views = nil },
		"empty-nonempty-root": func(value *DocVisibleGeneration) { value.Views["chat"] = DocVisibleRoot{} },
		"invalid-root-hash":   func(value *DocVisibleGeneration) { value.Views["chat"] = DocVisibleRoot{Hash: "bad", Units: 1} },
		"negative-units": func(value *DocVisibleGeneration) {
			root := value.Views["chat"]
			root.Units = -1
			value.Views["chat"] = root
		},
		"too-many-units": func(value *DocVisibleGeneration) {
			root := value.Views["chat"]
			root.Units = value.Events + 1
			value.Views["chat"] = root
		},
		"all-not-event-count": func(value *DocVisibleGeneration) { root := value.Views["all"]; root.Units--; value.Views["all"] = root },
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			candidate := docVisibleTestClone(generation)
			mutate(&candidate)
			if !errors.Is(candidate.Validate(generation.Hash, generation.RangeGeneration), ErrIntegrity) {
				test.Fatal("invalid descriptor validated")
			}
			_ = docVisibleTestJSON(test, candidate)
		})
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	_ = map[string][]byte{
		"empty":                   nil,
		"oversized":               bytes.Repeat([]byte(" "), MaxDocVisibleGenerationBytes+1),
		"invalid-json":            []byte("{"),
		"null":                    []byte("null"),
		"noncanonical-whitespace": append([]byte(" "), raw...),
		"unknown-field":           append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"unknown":0}`)...),
		"duplicate-field":         append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"version":1}`)...),
	}
}

func TestDocVisibleGenerationRequiresVerifiedPresentationMetadata(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}, docVisibleTestMessage(RoleAssistant, "answer")})
	missingPresentation := fixture.streamed
	missingPresentation.presentationAlgorithm = 0
	futurePresentation := fixture.streamed
	futurePresentation.presentationAlgorithm = DocVisibleAlgorithm + 1
	missingReader := fixture.streamed
	missingReader.read = nil
	for name, verified := range map[string]VerifiedDocRangeIndex{"zero": {}, "header-only": {index: fixture.streamed.Header()}, "legacy": missingPresentation, "future": futurePresentation, "missing-reader": missingReader} {
		test.Run(name, func(test *testing.T) {
			proof, objects, err := BuildVerifiedDocRangeGeneration(context.Background(), verified)
			if err != nil {
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(proof, VerifiedDocRangeGeneration{}) || len(objects) != 0 {
					test.Fatalf("invalid index factory returned proof or objects: %+v, %d objects, %v", proof.Descriptor(), len(objects), err)
				}
				return
			}
			emitted := 0
			generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error {
				emitted++
				return nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 0 {
				test.Fatalf("unverified presentation emitted %d objects: %+v, %v", emitted, generation, err)
			}
		})
	}
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	invalidHash := proof
	invalidHash.hash = "bad"
	for _, invalid := range []struct {
		name  string
		proof VerifiedDocRangeGeneration
		emit  func(context.Context, DocRangeObject) error
	}{
		{name: "nil-emitter", proof: proof},
		{name: "invalid-ranges", proof: invalidHash, emit: func(context.Context, DocRangeObject) error {
			test.Fatal("invalid range binding emitted an object")
			return nil
		}},
	} {
		test.Run(invalid.name, func(test *testing.T) {
			generation, err := BuildDocVisibleGeneration(context.Background(), invalid.proof, invalid.emit)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("invalid builder arguments returned generation: %+v, %v", generation, err)
			}
		})
	}
	for _, failure := range []string{"page-load", "page-hash", "rendered-rank"} {
		test.Run(failure, func(test *testing.T) {
			verified := fixture.streamed
			verified.index = fixture.streamed.Header()
			raw := docRangeTestPage(test, fixture.streamed, 0)
			want := ErrIntegrity
			switch failure {
			case "page-load":
				want = errors.New("source metadata page unavailable")
			case "page-hash":
				raw[0] = 'x'
			case "rendered-rank":
				entries := append([]DocRangeEntry(nil), fixture.entries...)
				entries[2].RenderedBefore = 0
				raw = docVisibleTestJSON(test, entries)
				verified.index.Pages[0] = HashContent(raw)
			}
			verified.read = func(context.Context, int) ([]byte, error) {
				if failure == "page-load" {
					return nil, want
				}
				return raw, nil
			}
			proof := docVisibleTestRangeProof(test, verified)
			generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
			if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("bad metadata returned a generation: %+v, %v", generation, err)
			}
		})
	}
}

func TestDocVisibleGenerationSealedRangeBindingOwnsDescriptor(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRendererEvents())
	proof, objects, err := BuildVerifiedDocRangeGeneration(context.Background(), fixture.streamed)
	if err != nil {
		test.Fatal(err)
	}
	want, wantObjects, err := BuildDocRangeGeneration(context.Background(), fixture.streamed)
	if err != nil || !reflect.DeepEqual(proof.Descriptor(), want) || !reflect.DeepEqual(objects, wantObjects) {
		test.Fatalf("sealed factory changed existing range generation or objects: %v", err)
	}
	raw, err := want.Encode()
	if err != nil {
		test.Fatal(err)
	}
	copy := proof.Descriptor()
	copy.Envelope[0] = 'x'
	copy.EventPages.Hash = HashContent([]byte("caller replacement event directory"))
	copy.ChunkPages.Hash = HashContent([]byte("caller replacement chunk directory"))
	copy.Hash = HashContent([]byte("caller replacement document"))
	for _, object := range objects {
		object.Bytes[0] = 'x'
	}
	if !reflect.DeepEqual(proof.Descriptor(), want) {
		test.Fatal("caller-owned descriptor or emitted bytes changed sealed range generation")
	}
	generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
	if err != nil || generation.Hash != want.Hash || generation.RangeGeneration != HashContent(raw) {
		test.Fatalf("visible generation lost sealed index/descriptor binding: %+v, %v", generation, err)
	}
	otherFixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "different document")})
	otherProof := docVisibleTestRangeProof(test, otherFixture.streamed)
	otherRaw, err := otherProof.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	other, err := BuildDocVisibleGeneration(context.Background(), otherProof, func(context.Context, DocRangeObject) error { return nil })
	if err != nil || other.Hash == generation.Hash || other.RangeGeneration == generation.RangeGeneration || other.RangeGeneration != HashContent(otherRaw) {
		test.Fatalf("distinct sealed documents shared a range binding: %+v, %v", other, err)
	}
}

func TestDocVisibleGenerationRejectsUnverifiedAndDecodedWrappers(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt")})
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	descriptor := proof.Descriptor()
	raw, err := descriptor.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if err != nil {
		test.Fatal(err)
	}
	encodedProof, err := json.Marshal(proof)
	if err != nil {
		test.Fatal(err)
	}
	wrappers := map[string]VerifiedDocRangeGeneration{
		"zero":                  {},
		"unverified-descriptor": {generation: descriptor},
		"index-only":            {index: fixture.streamed},
	}
	for name, input := range map[string][]byte{"json-proof": encodedProof, "json-descriptor": raw, "json-empty": []byte("{}"), "json-null": []byte("null")} {
		var wrapper VerifiedDocRangeGeneration
		if err := json.Unmarshal(input, &wrapper); err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(wrapper, VerifiedDocRangeGeneration{}) {
			test.Fatalf("%s created private verification state", name)
		}
		wrappers[name] = wrapper
	}
	for name, wrapper := range wrappers {
		test.Run(name, func(test *testing.T) {
			emitted := 0
			generation, err := BuildDocVisibleGeneration(context.Background(), wrapper, func(context.Context, DocRangeObject) error {
				emitted++
				return nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 0 {
				test.Fatalf("unverified wrapper emitted %d objects or returned a generation: %+v, %v", emitted, generation, err)
			}
		})
	}
}

func TestDocVisibleGenerationSealedFactoryCancellationReturnsNoProof(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRendererEvents())
	checks := 0
	_, _, err := BuildVerifiedDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, fixture.streamed)
	if err != nil || checks < 2 {
		test.Fatalf("sealed factory cancellation checkpoints: %d, %v", checks, err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		test.Run(fmt.Sprint(cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := 0
			proof, objects, err := BuildVerifiedDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}, fixture.streamed)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(proof, VerifiedDocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("canceled factory returned proof or objects: %+v, %d objects, %v", proof.Descriptor(), len(objects), err)
			}
		})
	}
}

func TestDocVisibleGenerationCancellationAndEmitFailuresReturnNoGeneration(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}, {Kind: EventToolResult, Output: "work"}, docVisibleTestMessage(RoleAssistant, "answer")})
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	checks, objects := 0, 0
	_, err := BuildDocVisibleGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof, func(context.Context, DocRangeObject) error {
		objects++
		return nil
	})
	if err != nil || checks < 2 || objects < 2 {
		test.Fatalf("baseline build: %d checks/%d objects, %v", checks, objects, err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		test.Run(fmt.Sprintf("checkpoint-%d", cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := 0
			generation, err := BuildDocVisibleGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}, proof, func(context.Context, DocRangeObject) error { return nil })
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("checkpoint %d returned a generation: %+v, %v", cancelAt, generation, err)
			}
		})
	}
	for _, failure := range []string{"emit-error", "emit-cancellation"} {
		for failAt := 1; failAt <= objects; failAt++ {
			test.Run(fmt.Sprintf("%s-%d", failure, failAt), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				seen := 0
				want := errors.New("object storage failed")
				if failure == "emit-cancellation" {
					want = context.Canceled
				}
				generation, err := BuildDocVisibleGeneration(ctx, proof, func(context.Context, DocRangeObject) error {
					seen++
					if seen == failAt {
						if failure == "emit-cancellation" {
							cancel()
							return nil
						}
						return want
					}
					return nil
				})
				if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || seen != failAt {
					test.Fatalf("failed emitter continued or returned a generation: %d/%d, %+v, %v", seen, failAt, generation, err)
				}
			})
		}
	}
}

func TestDocVisibleGenerationStreamingPageFailureStopsBeforeNextSourcePage(test *testing.T) {
	events := make([]CIREvent, DocRangePageEvents+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	fixture := docVisibleTestFixture(test, events)
	for _, failure := range []string{"emit-error", "emit-cancellation"} {
		test.Run(failure, func(test *testing.T) {
			verified := fixture.streamed
			read := verified.read
			pages, emitted := 0, 0
			verified.read = func(ctx context.Context, position int) ([]byte, error) {
				pages++
				return read(ctx, position)
			}
			proof := docVisibleTestRangeProof(test, verified)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("streaming page emit failed")
			if failure == "emit-cancellation" {
				want = context.Canceled
			}
			generation, err := BuildDocVisibleGeneration(ctx, proof, func(context.Context, DocRangeObject) error {
				emitted++
				if failure == "emit-cancellation" {
					cancel()
					return nil
				}
				return want
			})
			if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 1 || pages != 1 {
				test.Fatalf("streaming failure continued or returned generation: pages=%d emitted=%d, %+v, %v", pages, emitted, generation, err)
			}
		})
	}
}
