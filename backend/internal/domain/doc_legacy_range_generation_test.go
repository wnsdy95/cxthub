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

func legacyDocRangeGenerationPages(test testing.TB, generation DocRangeGeneration, objects []DocRangeObject) []DocRangeEntry {
	test.Helper()
	bodies := make(map[ContentHash][]byte, len(objects))
	for _, object := range objects {
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangePageBytes || HashContent(object.Bytes) != object.Hash {
			test.Fatalf("invalid generated object %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		if previous, exists := bodies[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatalf("inconsistent duplicate object %s", object.Hash)
		}
		bodies[object.Hash] = object.Bytes
	}
	visited := make(map[ContentHash]bool, len(objects))
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, exists := bodies[hash]
		if !exists || len(raw) > MaxDocRangeDirectoryBytes {
			test.Fatalf("missing or oversized directory %s (%d bytes)", hash, len(raw))
		}
		visited[hash] = true
		return raw, nil
	}
	entries := make([]DocRangeEntry, 0, int(generation.EventCount))
	for position := 0; position < generation.EventPages.Count; position++ {
		hash, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, load)
		if err != nil {
			test.Fatalf("resolve event page %d: %v", position, err)
		}
		raw, exists := bodies[hash]
		if !exists {
			test.Fatalf("missing event page %d: %s", position, hash)
		}
		visited[hash] = true
		page, err := generation.DecodePage(position, hash, raw)
		if err != nil {
			test.Fatalf("decode event page %d: %v", position, err)
		}
		entries = append(entries, page...)
		corrupted := bytes.Clone(raw)
		corrupted[len(corrupted)/2] ^= 1
		if _, err := generation.DecodePage(position, hash, corrupted); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("corrupt event page %d accepted: %v", position, err)
		}
		if _, err := generation.DecodePage(position, HashContent([]byte("different page")), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("wrong event page hash %d accepted: %v", position, err)
		}
	}
	if len(visited) != len(bodies) || len(entries) != int(generation.EventCount) {
		test.Fatalf("generation contains unreachable objects or wrong event count: visited=%d objects=%d events=%d", len(visited), len(bodies), len(entries))
	}
	expectedObjects := generation.EventPages.Count
	for children := generation.EventPages.Count; children > 0; {
		parents := (children + DocRangeDirectoryFanout - 1) / DocRangeDirectoryFanout
		expectedObjects += parents
		if parents == 1 {
			break
		}
		children = parents
	}
	if len(objects) != expectedObjects {
		test.Fatalf("generation emitted non-metadata objects: got %d, want %d", len(objects), expectedObjects)
	}
	if generation.EventPages.Count > 0 {
		if _, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, 0, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			raw, err := load(ctx, hash)
			corrupted := bytes.Clone(raw)
			corrupted[0] ^= 1
			return corrupted, err
		}); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("corrupt directory accepted: %v", err)
		}
	}
	for _, position := range []int{-1, generation.EventPages.Count} {
		if _, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, load); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid event page position %d accepted: %v", position, err)
		}
	}
	if _, err := generation.DecodeChunks(0, HashContent([]byte("[]")), []byte("[]")); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy generation exposed a conversation chunk page: %v", err)
	}
	return entries
}

func TestLegacyDocRangeGenerationCanonicalMetadataAndIdentity(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents, DocRangePageEvents + 1, 2*DocRangePageEvents + 7} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			verified, original := legacyDocRangeGenerationFixture(test, count)
			originalRef := verified.DocumentRef()
			var document struct {
				Envelope json.RawMessage   `json:"envelope"`
				Events   []json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(original, &document); err != nil {
				test.Fatal(err)
			}
			generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
			if err != nil {
				test.Fatal(err)
			}
			if generation.Version != 3 || generation.SourceIdentity == nil || *generation.SourceIdentity != DocumentIdentityLegacy ||
				generation.Identity() != DocumentIdentityLegacy || generation.Algorithm != 1 || generation.Hash != HashContent(original) ||
				generation.EventCount != int64(count) || !bytes.Equal(generation.Envelope, document.Envelope) ||
				generation.EventPages.Count != (count+DocRangePageEvents-1)/DocRangePageEvents ||
				generation.ChunkCount() != 0 || generation.ChunkPages != (DocRangeDirectoryRoot{}) {
				test.Fatalf("legacy generation changed source identity or metadata: %+v", generation)
			}
			expectedStreamBytes := max(0, len(document.Events)-1)
			for _, raw := range document.Events {
				expectedStreamBytes += len(raw)
			}
			if generation.StreamBytes != int64(expectedStreamBytes) {
				test.Errorf("legacy stream byte count = %d, want exact original stream length %d", generation.StreamBytes, expectedStreamBytes)
			}
			encoded, err := generation.Encode()
			if err != nil || len(encoded) > MaxDocRangeGenerationBytes || !bytes.Contains(encoded, []byte(`"source_identity":""`)) {
				test.Fatalf("legacy descriptor encoding: %s, %v", encoded, err)
			}
			decoded, err := DecodeDocRangeGeneration(verified.Hash(), HashContent(encoded), encoded)
			if err != nil || !reflect.DeepEqual(decoded, generation) {
				test.Fatalf("legacy descriptor round trip: %v", err)
			}
			entries := legacyDocRangeGenerationPages(test, decoded, objects)
			offset, rendered := 0, 0
			for position, entry := range entries {
				var event CIREvent
				if err := json.Unmarshal(document.Events[position], &event); err != nil {
					test.Fatal(err)
				}
				seed := event.Kind == EventMessage && len(event.Blocks) > 0 && strings.HasPrefix(strings.TrimLeft(event.Blocks[0].Text, " \ufeff"), "[cxt seed] Branch-switch context:")
				empty := event.Kind == EventReasoning && strings.Trim(event.RedactedSummary, " \ufeff\t\n") == ""
				want := DocRangeEntry{DocEventIndex: DocEventIndex{Index: position, Offset: offset, Length: len(document.Events[position]),
					Hash: HashContent(document.Events[position]), Seq: event.Seq, Role: string(event.Role)},
					Kind: event.Kind, ToolName: event.ToolName, CompactSummary: event.CompactSummary,
					SeedSummary: seed, EmptyReasoning: empty, RenderedBefore: rendered}
				if entry != want {
					test.Fatalf("original event %d metadata changed: got %+v, want %+v", position, entry, want)
				}
				offset += len(document.Events[position]) + 1
				if !empty {
					rendered++
				}
			}
			for _, object := range append(objects, DocRangeObject{Bytes: encoded}) {
				for _, forbidden := range []string{`"locked"`, "opaque-locked-only", "public-message-only", "public-seed-only", "public-summary-only", "private-tool-input-only", "private-tool-output-only"} {
					if bytes.Contains(object.Bytes, []byte(forbidden)) {
						test.Fatalf("public generation retained event payload %q", forbidden)
					}
				}
			}
			repeated, repeatedObjects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
			if err != nil || !reflect.DeepEqual(repeated, generation) || !reflect.DeepEqual(repeatedObjects, objects) ||
				verified.DocumentRef() != originalRef || !bytes.Equal(verified.Bytes(), original) {
				test.Fatalf("generation rewrote its original proof or produced nondeterministic output: %v", err)
			}
			if _, exists := verified.ConversationManifest(); exists {
				test.Fatal("legacy proof acquired a conversation root manifest")
			}
		})
	}
}

func TestLegacyDocRangeGenerationLegacyProofRepresentationParity(test *testing.T) {
	for _, count := range []int{1, DocRangePageEvents + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			verified, original := legacyDocRangeGenerationFixture(test, count)
			baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
			if err != nil {
				test.Fatal(err)
			}
			var verifier CanonicalDocVerifier
			canonical, err := verifier.Verify(context.Background(), verified.Hash(), original)
			if err != nil {
				test.Fatal(err)
			}
			proofs := map[string]VerifiedSessionDoc{"canonical": canonical}
			for name, plan := range map[string]func([]byte) (DocChunkPlan, bool){"chunk-v1": PlanDocChunksV1, "chunk-v2": PlanDocChunks} {
				chunks, ok := plan(original)
				if !ok {
					test.Fatalf("legacy fixture could not use %s", name)
				}
				proof, err := verifier.VerifyChunks(context.Background(), verified.Hash(), chunks.Manifest, manifestProofLoad(chunks.Bodies))
				if err != nil {
					test.Fatal(err)
				}
				proofs[name] = proof
			}
			for name, proof := range proofs {
				test.Run(name, func(test *testing.T) {
					generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
					if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) ||
						proof.DocumentRef() != verified.DocumentRef() || !bytes.Equal(proof.Bytes(), original) {
						test.Fatalf("legacy proof representation changed generation or original identity: %v", err)
					}
				})
			}
		})
	}
}

func TestLegacyDocRangeGenerationStreamedParityAndIdentity(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents + 1, 2*DocRangePageEvents + 7} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			materialized, original := legacyDocRangeGenerationFixture(test, count)
			baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), materialized)
			if err != nil {
				test.Fatal(err)
			}
			baselineBytes, err := baseline.Encode()
			if err != nil {
				test.Fatal(err)
			}
			for _, width := range []int{31, ChunkTarget} {
				test.Run(fmt.Sprint(width), func(test *testing.T) {
					proof, spool := legacyDocRangeGenerationStreamFixture(test, original, width)
					originalRef, originalManifest := proof.Reference().DocumentRef(), proof.Manifest()
					originalStream := bytes.Clone(spool.body)
					generation, objects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
					if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) {
						test.Fatalf("streamed generation differs from materialized descriptor or object bytes: %v", err)
					}
					encoded, err := generation.Encode()
					if err != nil || !bytes.Equal(encoded, baselineBytes) || generation.Hash != HashContent(original) ||
						generation.Identity() != DocumentIdentityLegacy || generation.StreamBytes != int64(len(originalStream)) ||
						generation.ChunkCount() != 0 || generation.ChunkPages != (DocRangeDirectoryRoot{}) {
						test.Fatalf("streamed generation changed original identity or exact stream length: %v", err)
					}
					decoded, err := DecodeDocRangeGeneration(materialized.Hash(), HashContent(encoded), encoded)
					if err != nil || !reflect.DeepEqual(decoded, baseline) {
						test.Fatalf("streamed descriptor round trip: %v", err)
					}
					legacyDocRangeGenerationPages(test, generation, objects)
					*generation.SourceIdentity = DocumentIdentityRootV1
					generation.Envelope[0] = '!'
					for _, object := range objects {
						object.Bytes[0] = '!'
					}
					repeated, repeatedObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
					if err != nil || !reflect.DeepEqual(repeated, baseline) || !reflect.DeepEqual(repeatedObjects, baselineObjects) ||
						proof.Reference().DocumentRef() != originalRef || originalRef != materialized.DocumentRef() ||
						!reflect.DeepEqual(proof.Manifest(), originalManifest) || !bytes.Equal(spool.body, originalStream) ||
						!bytes.Equal(materialized.Bytes(), original) {
						test.Fatalf("streamed generation rewrote its original proof, spool, or subsequent output: %v", err)
					}
				})
			}
		})
	}
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
			generation, objects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if !errors.Is(err, ErrUnsupportedDocumentIdentity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("invalid streamed proof produced legacy metadata: %+v, %v", generation, err)
			}
		})
	}
}

func TestLegacyDocRangeGenerationStreamedCancellation(test *testing.T) {
	for _, count := range []int{0, 3} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			_, original := legacyDocRangeGenerationFixture(test, count)
			proof, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
			originalRef, originalManifest := proof.Reference().DocumentRef(), proof.Manifest()
			originalStream := bytes.Clone(spool.body)
			checks := 0
			baseline, baselineObjects, err := BuildStreamedLegacyDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof)
			if err != nil || checks < count+2 {
				test.Fatalf("missing streamed cancellation checkpoints: %d, %v", checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				ctx, cancel := context.WithCancel(context.Background())
				seen := 0
				_, _, err := BuildStreamedLegacyDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
					seen++
					if seen == cancelAt {
						cancel()
					}
				}}, proof)
				cancel()
				if !errors.Is(err, context.Canceled) {
					test.Fatalf("streamed checkpoint %d ignored cancellation: %v", cancelAt, err)
				}
			}
			rebuilt, rebuiltObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) ||
				proof.Reference().DocumentRef() != originalRef || !reflect.DeepEqual(proof.Manifest(), originalManifest) || !bytes.Equal(spool.body, originalStream) {
				test.Fatalf("streamed cancellation damaged original proof, spool, or later generation: %v", err)
			}
		})
	}
}

func TestLegacyDocRangeGenerationStreamedRejectsChangedSpoolReads(test *testing.T) {
	_, original := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	readFailure := errors.New("synthetic spool read failure")
	for _, fixture := range []struct {
		name   string
		change func(*testing.T, *legacyTestStream)
		want   error
	}{
		{name: "late-opaque-spool-mutation", want: ErrIntegrity, change: func(test *testing.T, spool *legacyTestStream) {
			position := bytes.LastIndex(spool.body, []byte("opaque-locked-only"))
			if position < 0 {
				test.Fatal("fixture has no opaque payload")
			}
			spool.body[position] = 'X'
		}},
		{name: "changed-opaque-read-bytes", want: ErrIntegrity, change: func(test *testing.T, spool *legacyTestStream) {
			spool.readTransform = func(body []byte) []byte {
				changed := bytes.Replace(body, []byte("opaque-locked-only"), []byte("opaque-locked-evil"), 1)
				if len(changed) != len(body) || !json.Valid(changed) {
					test.Fatal("opaque mutation must preserve event length and valid JSON")
				}
				return changed
			}
		}},
		{name: "short-read", want: ErrIntegrity, change: func(test *testing.T, spool *legacyTestStream) {
			spool.readTransform = func(body []byte) []byte { return body[:len(body)-1] }
		}},
		{name: "long-read", want: ErrIntegrity, change: func(test *testing.T, spool *legacyTestStream) {
			spool.readTransform = func(body []byte) []byte { return append(bytes.Clone(body), ' ') }
		}},
		{name: "read-failure", want: readFailure, change: func(test *testing.T, spool *legacyTestStream) { spool.readError = readFailure }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			proof, spool := legacyDocRangeGenerationStreamFixture(test, original, 31)
			originalRef, originalManifest := proof.Reference().DocumentRef(), proof.Manifest()
			originalStream := bytes.Clone(spool.body)
			baseline, baselineObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			fixture.change(test, spool)
			generation, objects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("changed spool read produced metadata: %+v, %v; want %v", generation, err, fixture.want)
			}
			spool.body, spool.readTransform, spool.readError = originalStream, nil, nil
			rebuilt, rebuiltObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) ||
				proof.Reference().DocumentRef() != originalRef || !reflect.DeepEqual(proof.Manifest(), originalManifest) {
				test.Fatalf("failed streamed generation damaged original identity or successful retry: %v", err)
			}
		})
	}
}

func TestLegacyDocRangeGenerationDirectoryFanout(test *testing.T) {
	count := DocRangePageEvents*DocRangeDirectoryFanout + 1
	raw := canonicalAdmissionDoc(test, canonicalAdmissionEvents(29, count))
	var verifier CanonicalDocVerifier
	verified, err := verifier.Verify(context.Background(), HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
	if err != nil || generation.EventPages.Count != DocRangeDirectoryFanout+1 {
		test.Fatalf("multi-level legacy directory: %+v, %v", generation.EventPages, err)
	}
	entries := legacyDocRangeGenerationPages(test, generation, objects)
	for position, entry := range entries {
		if entry.Index != position || entry.Seq != 29+position || entry.RenderedBefore != position || entry.Kind != EventMessage || entry.Role != string(RoleUser) || entry.Text != "" {
			test.Fatalf("multi-level directory changed original event %d: %+v", position, entry)
		}
	}
}

func TestLegacyDocRangeGenerationBoundsAndOwnedOutput(test *testing.T) {
	role := strings.Repeat("역🙂", 1024)
	toolName := strings.Repeat("도구🙂", 1024)
	payload := "opaque-locked-only" + strings.Repeat("x", 256<<10)
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 41, Role: Role(role), AgentMessage: true, Blocks: []ContentBlock{{Type: "text", Text: "visible-payload-only" + strings.Repeat("y", 256<<10)}},
			Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: payload}},
		{Kind: EventToolCall, Seq: 93, ToolName: toolName, Input: map[string]any{"payload": payload}},
	}}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	verified, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
	if err != nil {
		test.Fatal(err)
	}
	generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	entries := legacyDocRangeGenerationPages(test, generation, objects)
	if entries[0].Role != string([]rune(role)[:128]) || entries[1].ToolName != string([]rune(toolName)[:128]) {
		test.Fatal("public labels exceeded their bounded original prefixes")
	}
	encoded, err := generation.Encode()
	if err != nil || len(encoded) > MaxDocRangeGenerationBytes {
		test.Fatalf("oversized legacy descriptor: %d bytes, %v", len(encoded), err)
	}
	for _, object := range append(objects, DocRangeObject{Bytes: encoded}) {
		if len(object.Bytes) > MaxDocRangeDirectoryBytes || bytes.Contains(object.Bytes, []byte("opaque-locked-only")) || bytes.Contains(object.Bytes, []byte("visible-payload-only")) {
			test.Fatal("large event payload leaked into bounded public metadata")
		}
	}
	baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	*generation.SourceIdentity = DocumentIdentityRootV1
	generation.Envelope[0] = '!'
	for _, object := range objects {
		object.Bytes[0] = '!'
	}
	rebuilt, rebuiltObjects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
	if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) || !bytes.Equal(verified.Bytes(), raw) {
		test.Fatalf("returned metadata aliases another build or the original proof: %v", err)
	}
}

func TestLegacyDocRangeGenerationDescriptorSourceIdentityStrict(test *testing.T) {
	verified, _ := legacyDocRangeGenerationFixture(test, 1)
	generation, _, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for name, identity := range map[string]json.RawMessage{
		"missing": nil, "null": json.RawMessage(`null`), "root": docRangeGenerationJSON(test, DocumentIdentityRootV1),
		"unknown": json.RawMessage(`"future"`), "number": json.RawMessage(`0`), "object": json.RawMessage(`{}`),
	} {
		test.Run(name, func(test *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				test.Fatal(err)
			}
			if identity == nil {
				delete(fields, "source_identity")
			} else {
				fields["source_identity"] = identity
			}
			raw := docRangeGenerationJSON(test, fields)
			decoded, err := DecodeDocRangeGeneration(verified.Hash(), HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("invalid version 3 source identity decoded: %+v, %v", decoded, err)
			}
		})
	}
	for _, identity := range []*DocumentIdentity{nil, new(DocumentIdentityRootV1)} {
		invalid := generation
		invalid.SourceIdentity = identity
		if err := invalid.Validate(verified.Hash()); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid version 3 source identity validated: %v", err)
		}
		if raw, err := invalid.Encode(); !errors.Is(err, ErrIntegrity) || raw != nil {
			test.Fatalf("invalid version 3 source identity encoded: %s, %v", raw, err)
		}
	}
	if _, err := DecodeDocRangeGeneration(HashContent([]byte("different document")), HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy descriptor accepted another source hash: %v", err)
	}
	if _, err := DecodeDocRangeGeneration(verified.Hash(), HashContent([]byte("different descriptor")), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy descriptor accepted another descriptor hash: %v", err)
	}
}

func TestLegacyDocRangeGenerationRejectsRootAndInvalidProof(test *testing.T) {
	legacy, original := legacyDocRangeGenerationFixture(test, 1)
	var document CIRDocument
	if err := json.Unmarshal(original, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil || !root.Valid() || root.Hash() == legacy.Hash() || root.DocumentRef().Identity != DocumentIdentityRootV1 {
		test.Fatalf("root proof fixture: %v", err)
	}
	for name, proof := range map[string]VerifiedSessionDoc{"root": root, "zero": {}, "hash-only": {hash: legacy.Hash()}} {
		test.Run(name, func(test *testing.T) {
			generation, objects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
			if !errors.Is(err, ErrUnsupportedDocumentIdentity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("unsupported proof produced legacy metadata: %+v, %v", generation, err)
			}
		})
	}
	if !bytes.Equal(root.Bytes(), original) || !bytes.Equal(legacy.Bytes(), original) || legacy.DocumentRef().Identity != DocumentIdentityLegacy {
		test.Fatal("legacy generation rejection changed original proof identity or bytes")
	}
}

func TestLegacyDocRangeGenerationCancellation(test *testing.T) {
	for _, count := range []int{0, 3} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			verified, original := legacyDocRangeGenerationFixture(test, count)
			checks := 0
			baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, verified)
			if err != nil || checks < count+2 {
				test.Fatalf("missing cancellation checkpoints: %d, %v", checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				ctx, cancel := context.WithCancel(context.Background())
				seen := 0
				_, _, err := BuildLegacyDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
					seen++
					if seen == cancelAt {
						cancel()
					}
				}}, verified)
				cancel()
				if !errors.Is(err, context.Canceled) {
					test.Fatalf("checkpoint %d ignored cancellation: %v", cancelAt, err)
				}
			}
			rebuilt, rebuiltObjects, err := BuildLegacyDocRangeGeneration(context.Background(), verified)
			if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) || !bytes.Equal(verified.Bytes(), original) {
				test.Fatalf("cancellation damaged original proof or later generation: %v", err)
			}
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
			previous := docRangeGenerationJSON(test, struct {
				Version     int                   `json:"version"`
				Algorithm   int                   `json:"algorithm"`
				Hash        ContentHash           `json:"hash"`
				Envelope    json.RawMessage       `json:"envelope"`
				EventCount  int64                 `json:"event_count"`
				StreamBytes int64                 `json:"stream_bytes"`
				EventPages  DocRangeDirectoryRoot `json:"event_pages"`
				ChunkPages  DocRangeDirectoryRoot `json:"chunk_pages"`
			}{generation.Version, generation.Algorithm, generation.Hash, generation.Envelope, generation.EventCount, generation.StreamBytes, generation.EventPages, generation.ChunkPages})
			decoded, err := DecodeDocRangeGeneration(generation.Hash, HashContent(previous), previous)
			if err != nil || !bytes.Equal(encoded, previous) || !reflect.DeepEqual(decoded, generation) {
				test.Fatalf("version 2 descriptor is no longer byte compatible: %v", err)
			}
			identity := DocumentIdentityLegacy
			generation.SourceIdentity = &identity
			if err := generation.Validate(generation.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("version 2 accepted an explicit legacy source identity: %v", err)
			}
		})
	}
}
