package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

type legacyTestStream struct {
	body          []byte
	appendError   error
	readError     error
	mutateAppend  bool
	readTransform func([]byte) []byte
	maxAppend     int
}

func (stream *legacyTestStream) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stream.appendError != nil {
		return stream.appendError
	}
	stream.maxAppend = max(stream.maxAppend, len(body))
	stream.body = append(stream.body, body...)
	if stream.mutateAppend {
		clear(body)
	}
	return nil
}

func (stream *legacyTestStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stream.readError != nil {
		return nil, stream.readError
	}
	if offset < 0 || length < 0 || offset > len(stream.body) || length > len(stream.body)-offset {
		return nil, ErrIntegrity
	}
	body := stream.body[offset : offset+length]
	if stream.readTransform != nil {
		body = stream.readTransform(body)
	}
	return body, nil
}

func TestLegacyStreamOracleParity(t *testing.T) {
	raw := verifierFixture(t)
	envelope, events := chunkVerifierParts(t, raw)
	for _, candidate := range []struct {
		name     string
		envelope []byte
		events   []byte
	}{
		{"valid", envelope, events},
		{"empty", envelope, nil},
		{"equal-sequences", envelope, bytes.Replace(events, []byte(`"seq":1`), []byte(`"seq":0`), 1)},
		{"regressing-sequence", envelope, bytes.Replace(events, []byte(`"seq":2`), []byte(`"seq":0`), 1)},
		{"negative-sequence", envelope, bytes.Replace(events, []byte(`"seq":1`), []byte(`"seq":-1`), 1)},
		{"precision", envelope, bytes.Replace(events, []byte(`9007199254740992`), []byte(`9007199254740993`), 1)},
		{"exponent", envelope, bytes.Replace(events, []byte(`9007199254740992`), []byte(`1e0`), 1)},
		{"duplicate-field", envelope, bytes.Replace(events, []byte(`"seq":1`), []byte(`"seq":1,"seq":1`), 1)},
		{"unknown-field", envelope, bytes.Replace(events, []byte(`"seq":1`), []byte(`"seq":1,"unknown":true`), 1)},
		{"wrong-union", envelope, bytes.Replace(events, []byte(`"kind":"tool_call"`), []byte(`"kind":"message"`), 1)},
		{"null-blocks", envelope, bytes.Replace(events, []byte(`"blocks":[]`), []byte(`"blocks":null`), 1)},
		{"internal-whitespace", envelope, bytes.Replace(events, []byte(`"seq":1`), []byte(`"seq": 1`), 1)},
		{"between-whitespace", envelope, bytes.Replace(events, []byte(`},{`), []byte(`}, {`), 1)},
		{"leading-comma", envelope, append([]byte{','}, events...)},
		{"trailing-comma", envelope, append(bytes.Clone(events), ',')},
		{"missing-comma", envelope, bytes.Replace(events, []byte(`},{`), []byte(`}{`), 1)},
		{"double-comma", envelope, bytes.Replace(events, []byte(`},{`), []byte(`},,{`), 1)},
		{"truncated", envelope, events[:len(events)-1]},
		{"mismatched-close", envelope, append(bytes.Clone(events[:len(events)-1]), ']')},
		{"wrapper-injection", envelope, append(bytes.Clone(events), []byte(`],"events":[]`)...)},
		{"invalid-escape", envelope, bytes.Replace(events, []byte(`original`), []byte(`\q`), 1)},
		{"invalid-utf8", envelope, bytes.Replace(events, []byte(`original`), []byte{0xff}, 1)},
		{"primitive", envelope, []byte(`null`)},
		{"array", envelope, []byte(`[]`)},
		{"unknown-version", bytes.Replace(envelope, []byte(`"2"`), []byte(`"99"`), 1), events},
		{"v1-union", bytes.Replace(envelope, []byte(`"2"`), []byte(`"1"`), 1), events},
		{"duplicate-envelope", bytes.Replace(envelope, []byte(`"cir_version":"2"`), []byte(`"cir_version":"2","cir_version":"2"`), 1), events},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			canonical := AssembleCanonicalDoc(candidate.envelope, [][]byte{candidate.events})
			oracle := canonicalVerificationOracle(canonical)
			for _, width := range []int{1, 7, ChunkTarget} {
				input := chunkVerifierPlan(candidate.envelope, candidate.events, width)
				for _, warm := range []bool{false, true} {
					var verifier CanonicalDocVerifier
					if warm {
						if _, err := verifier.Verify(context.Background(), HashContent(raw), raw); err != nil {
							t.Fatal(err)
						}
					}
					before, order, next := maps.Clone(verifier.events), slices.Clone(verifier.order), verifier.next
					spool := &legacyTestStream{}
					proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(canonical), input.Manifest, chunkVerifierLoad(input), spool)
					if (err == nil) != (oracle == nil) {
						t.Fatalf("width=%d warm=%t: stream=%v oracle=%v", width, warm, err, oracle)
					}
					if err != nil {
						if proof.Valid() || proof.Reference().Valid() || !maps.Equal(before, verifier.events) || !slices.Equal(order, verifier.order) || next != verifier.next {
							t.Fatal("failed document admitted a proof")
						}
						continue
					}
					if !proof.Valid() || proof.Reference().Hash() != HashContent(canonical) || proof.Reference().DocumentRef().Identity != DocumentIdentityLegacy || !bytes.Equal(spool.body, candidate.events) {
						t.Fatal("canonical legacy identity or spooled stream changed")
					}
					plan, err := proof.PlanReadIndexContext(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					index, err := plan.Build(nil)
					if err != nil || index.Events == nil {
						t.Fatal("verified stream cannot build its index", err)
					}
				}
			}
		})
	}
}

func TestLegacyStreamNormalizedChunksAndReadBlocks(t *testing.T) {
	for _, seam := range []string{"\\\"", "\ud55c\uae00", "\n\t"} {
		t.Run(fmt.Sprintf("%q", seam), func(t *testing.T) {
			document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"original"}}, Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "MARKER"}}}}}
			initial, err := CanonicalBytes(document)
			if err != nil {
				t.Fatal(err)
			}
			_, initialEvents := chunkVerifierParts(t, initial)
			textOffset := bytes.Index(initialEvents, []byte("MARKER"))
			document.Events[0].Blocks[0].Text = strings.Repeat("x", ChunkTarget-textOffset-1) + seam + strings.Repeat("MixedCase ", ChunkTarget/5)
			for ordinal := 1; ordinal < ReadIndexBlockEvents+3; ordinal++ {
				document.Events = append(document.Events, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: ordinal * 3, Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("text %d", ordinal)}}})
			}
			raw, err := CanonicalBytes(document)
			if err != nil {
				t.Fatal(err)
			}
			envelope, events := chunkVerifierParts(t, raw)
			normalized, _ := PlanDocChunks(raw)
			legacy, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
			if err != nil {
				t.Fatal(err)
			}
			want, err := legacy.ReadIndex()
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range []int{ChunkTarget - 3, len(events)} {
				input := chunkVerifierPlan(envelope, events, width)
				var verifier CanonicalDocVerifier
				for scan := 0; scan < 2; scan++ {
					spool := &legacyTestStream{mutateAppend: true}
					proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), spool)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(proof.Manifest(), normalized.Manifest) || spool.maxAppend > canonicalCheckBytes {
						t.Fatal("partition normalization or append bounds changed")
					}
					for ordinal, hash := range normalized.Order {
						body, err := proof.Chunk(context.Background(), ordinal)
						if err != nil || !bytes.Equal(body, normalized.Bodies[hash]) {
							t.Fatal("normalized chunk differs", err)
						}
						clear(body)
					}
					manifest := proof.Manifest()
					clear(manifest.Envelope)
					manifest.Chunks[0] = "changed"
					plan, err := proof.PlanReadIndexContext(context.Background())
					if err != nil || plan.source != spool || len(plan.stream.parts) != 0 || !plan.sequencesKnown {
						t.Fatal("read plan did not preserve verified spool", err)
					}
					got, err := plan.BuildContext(context.Background(), nil)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatal("read index differs", err)
					}
					var combined []DocEventIndex
					for _, block := range plan.Blocks() {
						if block.plan.source != spool || !block.plan.sequencesKnown {
							t.Fatal("block dropped source or verified sequences")
						}
						index, err := block.BuildContext(context.Background(), nil)
						if err != nil {
							t.Fatal(err)
						}
						ranges, err := block.plan.BuildRangesContext(context.Background())
						if err != nil || ranges.Events[0].Seq != index.Events[0].Seq {
							t.Fatal("block ranges lost sequence", err)
						}
						for _, event := range index.Events {
							event.Index += block.FirstEvent
							event.Offset += block.Offset
							combined = append(combined, event)
						}
					}
					if !reflect.DeepEqual(combined, want.Events) {
						t.Fatal("block coordinate space or search projection changed")
					}
				}
			}
		})
	}
}

func TestLegacyStreamMutableSources(t *testing.T) {
	raw := verifierFixture(t)
	envelope, events := chunkVerifierParts(t, raw)
	input := chunkVerifierPlan(envelope, events, 7)
	scratch := make([]byte, 7)
	loads := 0
	loader := func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		body := input.Bodies[hash]
		copy(scratch, body)
		clear(input.Manifest.Envelope)
		clear(input.Manifest.Chunks)
		return scratch[:len(body)], nil
	}
	spool := &legacyTestStream{}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, loader, spool)
	if err != nil || loads != len(input.Order) {
		t.Fatal("loader reuse or manifest alias", err)
	}
	clear(scratch)
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Build(nil); err != nil {
		t.Fatal("loader reuse changed spool", err)
	}
	known := map[ContentHash]bool{}
	for _, hash := range plan.EventHashes() {
		known[hash] = true
	}
	spool.body[bytes.Index(spool.body, []byte("original"))] = 'X'
	if _, err := proof.Chunk(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("changed spool chunk accepted", err)
	}
	for name, build := range map[string]func(context.Context) (DocReadIndex, error){
		"full":     func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, nil) },
		"known":    func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, known) },
		"metadata": plan.BuildMetadataContext,
		"block":    func(ctx context.Context) (DocReadIndex, error) { return plan.Blocks()[0].BuildContext(ctx, known) },
	} {
		if _, err := build(context.Background()); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("%s trusted changed spool: %v", name, err)
		}
	}
	copy(spool.body, events)
	for _, lengthDelta := range []int{-1, 1} {
		spool.readTransform = func(body []byte) []byte {
			if lengthDelta < 0 {
				return body[:len(body)-1]
			}
			return append(bytes.Clone(body), ' ')
		}
		if _, err := proof.Chunk(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
			t.Fatal("wrong chunk length accepted", err)
		}
		if _, err := plan.Build(nil); !errors.Is(err, ErrIntegrity) {
			t.Fatal("wrong event length accepted", err)
		}
	}
	spool.readTransform = nil
	spool.readError = errors.New("closed spool")
	if _, err := proof.Chunk(context.Background(), 0); !errors.Is(err, spool.readError) {
		t.Fatal("lost spool read error", err)
	}
	if _, err := plan.Build(nil); !errors.Is(err, spool.readError) {
		t.Fatal("lost plan read error", err)
	}
}

func TestLegacyStreamCurrentOccurrencesAndFailures(t *testing.T) {
	raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(0, 1))
	envelope, event := chunkVerifierParts(t, raw)
	input := buildDocChunkPlan(ChunkFormatV2, envelope, [][]byte{event, []byte(","), event})
	canonical := AssembleCanonicalDoc(envelope, [][]byte{event, event})
	for _, warm := range []bool{false, true} {
		var verifier CanonicalDocVerifier
		if warm {
			if _, err := verifier.VerifyLegacyStream(context.Background(), HashContent(canonical), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{}); err != nil {
				t.Fatal(err)
			}
		}
		loads := 0
		proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(canonical), input.Manifest, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			body := bytes.Clone(input.Bodies[hash])
			if loads == 3 {
				body[len(body)-2] ^= 1
			}
			return body, nil
		}, &legacyTestStream{})
		if err == nil || proof.Valid() || loads != 3 {
			t.Fatal("repeated current chunk skipped", err)
		}
	}
	for _, operation := range []string{"identity", "chunk", "load", "append"} {
		t.Run(operation, func(t *testing.T) {
			var verifier CanonicalDocVerifier
			spool := &legacyTestStream{}
			target := HashContent(canonical)
			failure := errors.New("synthetic I/O error")
			load := chunkVerifierLoad(input)
			switch operation {
			case "identity":
				target = HashContent([]byte("different document"))
			case "chunk":
				input.Manifest.Chunks[0] = HashContent([]byte("different chunk"))
				defer func() { input.Manifest.Chunks[0] = input.Order[0] }()
			case "load":
				load = func(context.Context, ContentHash) ([]byte, error) { return nil, failure }
			case "append":
				spool.appendError = failure
			}
			proof, err := verifier.VerifyLegacyStream(context.Background(), target, input.Manifest, load, spool)
			if err == nil || proof.Valid() || len(verifier.events) != 0 {
				t.Fatal("failed verification admitted proof", err)
			}
			if (operation == "load" || operation == "append") && !errors.Is(err, failure) {
				t.Fatal("I/O error not preserved", err)
			}
		})
	}
}

func TestLegacyStreamCancellation(t *testing.T) {
	raw := verifierFixture(t)
	envelope, events := chunkVerifierParts(t, raw)
	input := chunkVerifierPlan(envelope, events, 37)
	checks := 0
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
	if err != nil {
		t.Fatal(err)
	}
	for checkpoint := 1; checkpoint <= checks; checkpoint++ {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		observed := canonicalAdmissionContext{Context: ctx, check: func() {
			calls++
			if calls == checkpoint {
				cancel()
			}
		}}
		var fresh CanonicalDocVerifier
		partial, err := fresh.VerifyLegacyStream(observed, HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
		cancel()
		if !errors.Is(err, context.Canceled) || partial.Valid() || len(fresh.events) != 0 || len(fresh.order) != 0 {
			t.Fatalf("checkpoint %d admitted canceled proof: %v", checkpoint, err)
		}
	}
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := proof.Chunk(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatal("chunk ignored cancellation", err)
	}
	if _, err := proof.PlanReadIndexContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("planning ignored cancellation", err)
	}
	if _, err := plan.Blocks()[0].BuildContext(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("block ignored cancellation", err)
	}
}

func TestLegacyStreamBoundsAndInvalidProofs(t *testing.T) {
	raw := verifierFixture(t)
	envelope, events := chunkVerifierParts(t, raw)
	for _, manifest := range []DocChunkManifest{
		{Format: "", Envelope: envelope},
		{Format: ChunkFormatV1, Envelope: envelope},
		{Format: "unknown", Envelope: envelope},
		{Format: ChunkFormatV2, Envelope: envelope, Chunks: make([]ContentHash, MaxDocJobChunks+1)},
		{Format: ChunkFormatV2, Envelope: bytes.Repeat([]byte{'x'}, MaxDocJobManifestBytes+1)},
		{Format: ChunkFormatV2, Envelope: envelope, Chunks: []ContentHash{"invalid"}},
	} {
		var verifier CanonicalDocVerifier
		proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), manifest, func(context.Context, ContentHash) ([]byte, error) {
			t.Fatal("loaded invalid manifest")
			return nil, nil
		}, &legacyTestStream{})
		if err == nil || proof.Valid() {
			t.Fatal("invalid metadata accepted")
		}
	}
	input := chunkVerifierPlan(envelope, events, 17)
	for _, limit := range []int{len(envelope), len(raw) - 1, len(raw), len(raw) + 1} {
		var verifier CanonicalDocVerifier
		proof, err := verifier.verifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{}, MaxLegacyStreamEventBytes, limit)
		if (err == nil) != (limit >= len(raw)) || (err != nil && (!errors.Is(err, ErrDocumentResourceLimit) || errors.Is(err, ErrIntegrity) || proof.Valid() || len(verifier.events) > 0)) {
			t.Fatalf("document limit %d: %v", limit, err)
		}
	}
	var invalid VerifiedLegacyStream
	if invalid.Valid() || invalid.Reference().Valid() {
		t.Fatal("zero value is a proof")
	}
	if _, err := invalid.Chunk(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("zero chunk accepted", err)
	}
	if _, err := invalid.PlanReadIndexContext(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatal("zero read plan accepted", err)
	}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ordinal := range []int{-1, len(proof.Manifest().Chunks), int(^uint(0) >> 1)} {
		if _, err := proof.Chunk(context.Background(), ordinal); !errors.Is(err, ErrIntegrity) {
			t.Fatal("out-of-range chunk accepted", err)
		}
	}
}

func TestLegacyStreamBlockVisitParityAndEarlyExit(t *testing.T) {
	raw := verifierFixture(t)
	input, _ := PlanDocChunks(raw)
	spool := &legacyTestStream{}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), spool)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	block := plan.Blocks()[0]
	for _, known := range []map[ContentHash]bool{nil, {block.EventHashes()[0]: true}} {
		want, err := block.BuildContext(context.Background(), known)
		if err != nil {
			t.Fatal(err)
		}
		var visited []DocEventIndex
		if err := block.VisitContext(context.Background(), known, func(event DocEventIndex) error {
			visited = append(visited, event)
			return nil
		}); err != nil || !reflect.DeepEqual(visited, want.Events) {
			t.Fatal("visitor differs from BuildContext", err)
		}
	}
	stop := errors.New("stop callback")
	visits := 0
	err = block.VisitContext(context.Background(), nil, func(DocEventIndex) error {
		visits++
		return stop
	})
	if !errors.Is(err, stop) || visits != 1 {
		t.Fatal("visitor did not propagate early exit", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	visits = 0
	err = block.VisitContext(ctx, nil, func(DocEventIndex) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatal("visitor did not stop after cancellation", err)
	}
	if err := block.VisitContext(context.Background(), nil, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatal("nil callback accepted", err)
	}
	spool.body[bytes.Index(spool.body, []byte("original"))] = '!'
	visits = 0
	err = block.VisitContext(context.Background(), nil, func(DocEventIndex) error {
		visits++
		return nil
	})
	if !errors.Is(err, ErrIntegrity) || visits != 0 {
		t.Fatal("visitor emitted corrupt event", err)
	}
}

func TestLegacyStreamConcurrentReadOwnership(t *testing.T) {
	raw := verifierFixture(t)
	input, _ := PlanDocChunks(raw)
	spool := &legacyTestStream{}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), spool)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := plan.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 5; attempt++ {
				manifest := proof.Manifest()
				clear(manifest.Envelope)
				clear(manifest.Chunks)
				chunk, err := proof.Chunk(context.Background(), 0)
				if err != nil {
					t.Error(err)
					return
				}
				clear(chunk)
				index, err := plan.Build(nil)
				if err != nil || !reflect.DeepEqual(index, want) {
					t.Error("concurrent output mutation changed source", err)
					return
				}
				index.Events[0].Hash = "changed"
			}
		}()
	}
	workers.Wait()
}
