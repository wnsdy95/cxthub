package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func chunkVerifierPlan(env, stream []byte, width int) DocChunkPlan {
	p := DocChunkPlan{Manifest: DocChunkManifest{Format: ChunkFormatV2, Envelope: bytes.Clone(env)}, Bodies: map[ContentHash][]byte{}}
	for len(stream) > 0 {
		body := bytes.Clone(stream[:min(width, len(stream))])
		h := HashContent(body)
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = body
		stream = stream[len(body):]
	}
	return p
}

func chunkVerifierLoad(p DocChunkPlan) func(context.Context, ContentHash) ([]byte, error) {
	return func(_ context.Context, h ContentHash) ([]byte, error) { return p.Bodies[h], nil }
}

func chunkVerifierParts(t testing.TB, raw []byte) ([]byte, []byte) {
	t.Helper()
	env, stream, ok := canonicalStream(raw)
	if !ok {
		t.Fatal("invalid fixture")
	}
	return env, stream
}

func TestCanonicalChunkVerifierOracleParity(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	for _, tc := range []struct {
		name        string
		env, stream []byte
	}{
		{"valid", env, stream},
		{"empty", env, nil},
		{"equal-sequences", env, bytes.Replace(stream, []byte(`"seq":1`), []byte(`"seq":0`), 1)},
		{"regressing-sequence", env, bytes.Replace(stream, []byte(`"seq":2`), []byte(`"seq":0`), 1)},
		{"negative-sequence", env, bytes.Replace(stream, []byte(`"seq":1`), []byte(`"seq":-1`), 1)},
		{"number-precision", env, bytes.Replace(stream, []byte(`9007199254740992`), []byte(`9007199254740993`), 1)},
		{"number-exponent", env, bytes.Replace(stream, []byte(`9007199254740992`), []byte(`1e0`), 1)},
		{"number-decimal", env, bytes.Replace(stream, []byte(`9007199254740992`), []byte(`1.0`), 1)},
		{"number-invalid", env, bytes.Replace(stream, []byte(`9007199254740992`), []byte(`01`), 1)},
		{"duplicate-key", env, bytes.Replace(stream, []byte(`"seq":1`), []byte(`"seq":1,"seq":1`), 1)},
		{"nested-duplicate-key", env, bytes.Replace(stream, []byte(`"n":9007199254740992`), []byte(`"n":0,"n":9007199254740992`), 1)},
		{"unknown-field", env, bytes.Replace(stream, []byte(`"seq":1`), []byte(`"seq":1,"unknown":true`), 1)},
		{"wrong-union", env, bytes.Replace(stream, []byte(`"kind":"tool_call"`), []byte(`"kind":"message"`), 1)},
		{"null-blocks", env, bytes.Replace(stream, []byte(`"blocks":[]`), []byte(`"blocks":null`), 1)},
		{"event-space", env, bytes.Replace(stream, []byte(`"seq":1`), []byte(`"seq": 1`), 1)},
		{"between-space", env, bytes.Replace(stream, []byte(`},{`), []byte(`}, {`), 1)},
		{"trailing-comma", env, append(bytes.Clone(stream), ',')},
		{"leading-comma", env, append([]byte{','}, stream...)},
		{"double-comma", env, bytes.Replace(stream, []byte(`},{`), []byte(`},,{`), 1)},
		{"missing-comma", env, bytes.Replace(stream, []byte(`},{`), []byte(`}{`), 1)},
		{"truncated", env, stream[:len(stream)-1]},
		{"mismatched-close", env, append(bytes.Clone(stream[:len(stream)-1]), ']')},
		{"wrapper-injection", env, append(bytes.Clone(stream), []byte(`],"events":[]`)...)},
		{"invalid-escape", env, bytes.Replace(stream, []byte(`original`), []byte(`\q`), 1)},
		{"primitive", env, []byte(`null`)},
		{"array-event", env, []byte(`[]`)},
		{"invalid-utf8", env, bytes.Replace(stream, []byte(`original`), []byte{0xff}, 1)},
		{"unknown-version", bytes.Replace(env, []byte(`"2"`), []byte(`"99"`), 1), stream},
		{"v1-presence", bytes.Replace(env, []byte(`"2"`), []byte(`"1"`), 1), stream},
		{"duplicate-envelope-key", bytes.Replace(env, []byte(`"cir_version":"2"`), []byte(`"cir_version":"2","cir_version":"2"`), 1), stream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := AssembleCanonicalDoc(tc.env, [][]byte{tc.stream})
			want := canonicalVerificationOracle(candidate)
			for _, width := range []int{1, 7, ChunkTarget} {
				p := chunkVerifierPlan(tc.env, tc.stream, width)
				for _, warm := range []bool{false, true} {
					var v CanonicalDocVerifier
					if warm {
						if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
							t.Fatal(err)
						}
					}
					got, err := v.VerifyChunks(context.Background(), HashContent(candidate), p.Manifest, chunkVerifierLoad(p))
					if (err == nil) != (want == nil) {
						t.Fatalf("width %d warm %t: got %v, oracle %v", width, warm, err, want)
					}
					if err == nil && (!got.Valid() || got.canonical != "" || got.chunks == nil || !bytes.Equal(got.Bytes(), candidate)) {
						t.Fatal("v2 identity/representation changed")
					}
					if err != nil && got.Valid() {
						t.Fatal("failed verification returned proof")
					}
				}
			}
		})
	}
}

func TestCanonicalChunkVerifierLargeBoundaryNormalizationAndReadPlan(t *testing.T) {
	for _, boundary := range []string{"\\\"", "\ud55c\uae00", "\n\t"} {
		t.Run(fmt.Sprintf("%q", boundary), func(t *testing.T) {
			cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"model"}}, Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "MARKER"}}}}}
			first, _ := CanonicalBytes(cir)
			_, initial := chunkVerifierParts(t, first)
			textOffset := bytes.Index(initial, []byte("MARKER"))
			cir.Events[0].Blocks[0].Text = strings.Repeat("x", ChunkTarget-textOffset-1) + boundary + strings.Repeat("tail ", MaxPortableChunkBytes/4)
			for i := 1; i < ReadIndexBlockEvents+3; i++ {
				cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: i})
			}
			raw, err := CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			env, stream := chunkVerifierParts(t, raw)
			standard, ok := PlanDocChunks(raw)
			if !ok {
				t.Fatal("fixture has no plan")
			}
			legacy, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
			if err != nil {
				t.Fatal(err)
			}
			wantIndex, err := legacy.ReadIndex()
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range []int{ChunkTarget - 3, len(stream)} { // includes an input body larger than 2 MiB
				p := chunkVerifierPlan(env, stream, width)
				var v CanonicalDocVerifier
				for scan := 0; scan < 2; scan++ {
					got, err := v.VerifyChunks(context.Background(), HashContent(raw), p.Manifest, chunkVerifierLoad(p))
					if err != nil {
						t.Fatal(err)
					}
					plan, ok := got.ChunkPlan()
					if !ok || !reflect.DeepEqual(plan, standard) {
						t.Fatal("nonstandard partition not normalized")
					}
					read, err := got.PlanReadIndex()
					if err != nil {
						t.Fatal(err)
					}
					idx, err := read.Build(nil)
					if err != nil || !reflect.DeepEqual(idx, wantIndex) {
						t.Fatal("segmented read plan differs", err)
					}
					known := map[ContentHash]bool{}
					for _, h := range read.EventHashes() {
						known[h] = true
					}
					reused, err := read.Build(known)
					if err != nil {
						t.Fatal(err)
					}
					for i, event := range wantIndex.Events {
						event.Text = ""
						if reused.Events[i] != event {
							t.Fatal("reuse changed coordinates")
						}
					}
					var blockEvents []DocEventIndex
					for _, b := range read.Blocks() {
						block, err := b.Build(nil)
						if err != nil {
							t.Fatal(err)
						}
						for _, event := range block.Events {
							event.Index += b.FirstEvent
							event.Offset += b.Offset
							blockEvents = append(blockEvents, event)
						}
					}
					if !reflect.DeepEqual(blockEvents, wantIndex.Events) {
						t.Fatal("block source spans changed")
					}
				}
			}
		})
	}
}

func TestCanonicalChunkVerifierOwnershipAndCurrentChunkReads(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	p := chunkVerifierPlan(env, stream, 17)
	var v CanonicalDocVerifier
	var buffer []byte
	loads := 0
	got, err := v.VerifyChunks(context.Background(), HashContent(raw), p.Manifest, func(_ context.Context, h ContentHash) ([]byte, error) {
		loads++
		buffer = append(buffer[:0], p.Bodies[h]...)
		// Mutating the original manifest during loading must not change the proof.
		p.Manifest.Envelope[0] = '!'
		p.Manifest.Chunks[0] = HashContent([]byte("tampered"))
		return buffer, nil
	})
	if err != nil || loads != len(p.Order) {
		t.Fatal("reused input buffer/manifest mutation changed verification", err)
	}
	clear(buffer)
	if !bytes.Equal(got.Bytes(), raw) {
		t.Fatal("retained loader buffer")
	}
	plan, ok := got.ChunkPlan()
	if !ok {
		t.Fatal("missing plan")
	}
	plan.Manifest.Envelope[0] = '!'
	plan.Manifest.Chunks[0] = "bad"
	for _, body := range plan.Bodies {
		clear(body)
	}
	plan.Order[0] = "bad"
	copy := got.Bytes()
	clear(copy)
	again, _ := got.ChunkPlan()
	want, _ := PlanDocChunks(raw)
	if !reflect.DeepEqual(again, want) || !bytes.Equal(got.Bytes(), raw) {
		t.Fatal("owned output aliases proof")
	}

	// Cache hits do not authorize missing, corrupt, or changed current chunks.
	for _, failure := range []string{"corrupt", "missing", "wrong-doc-hash", "wrong-chunk-hash"} {
		t.Run(failure, func(t *testing.T) {
			p := chunkVerifierPlan(env, stream, 17)
			want := HashContent(raw)
			if failure == "wrong-doc-hash" {
				want = HashContent([]byte("other"))
			}
			if failure == "wrong-chunk-hash" {
				p.Manifest.Chunks[len(p.Manifest.Chunks)-1] = HashContent([]byte("other"))
			}
			before := maps.Clone(v.events)
			missing := errors.New("missing chunk")
			calls := 0
			proof, err := v.VerifyChunks(context.Background(), want, p.Manifest, func(_ context.Context, h ContentHash) ([]byte, error) {
				calls++
				body := bytes.Clone(p.Bodies[h])
				if calls == len(p.Manifest.Chunks) {
					if failure == "corrupt" {
						body[0] ^= 1
					}
					if failure == "missing" {
						return nil, missing
					}
				}
				return body, nil
			})
			if err == nil || proof.Valid() || calls != len(p.Manifest.Chunks) || !maps.Equal(v.events, before) {
				t.Fatal("cached proof bypassed current reads", err)
			}
			if failure == "missing" && !errors.Is(err, missing) {
				t.Fatal("loader error lost", err)
			}
		})
	}
}

func TestCanonicalChunkVerifierEmptyAndV1Compatibility(t *testing.T) {
	raw := canonicalAdmissionDoc(t, nil)
	env, _ := chunkVerifierParts(t, raw)
	for _, chunks := range [][]ContentHash{nil, {HashContent(nil)}, {HashContent(nil), HashContent(nil)}} {
		var v CanonicalDocVerifier
		calls := 0
		got, err := v.VerifyChunks(context.Background(), HashContent(raw), DocChunkManifest{Format: ChunkFormatV2, Envelope: env, Chunks: chunks}, func(context.Context, ContentHash) ([]byte, error) { calls++; return nil, nil })
		if err != nil || !got.Valid() || !bytes.Equal(got.Bytes(), raw) || calls != len(chunks) {
			t.Fatal("empty v2 compatibility", err)
		}
		if _, ok := got.ChunkPlan(); ok {
			t.Fatal("empty stream has a chunk plan")
		}
		index, err := got.ReadIndex()
		if err != nil || len(index.Events) != 0 {
			t.Fatal("empty read index", err)
		}
	}
	// V1 stored bodies larger than the portable transport limit remain readable.
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("x", MaxPortableChunkBytes+1)}}}, {Kind: EventMessage, Role: RoleAssistant, Seq: 1}}}
	body, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	e, events, _ := splitCanonicalDocBytes(body)
	v1body := bytes.Join([][]byte{events[0], events[1]}, []byte{'\n'})
	for _, format := range []string{ChunkFormatV1, ""} {
		var v CanonicalDocVerifier
		got, err := v.VerifyChunks(context.Background(), HashContent(body), DocChunkManifest{Format: format, Envelope: e, Chunks: []ContentHash{HashContent(v1body)}}, func(context.Context, ContentHash) ([]byte, error) { return v1body, nil })
		if err != nil || !bytes.Equal(got.Bytes(), body) {
			t.Fatal("v1 compatibility failed", err)
		}
		plan, ok := got.ChunkPlan()
		want, _ := PlanDocChunks(body)
		if !ok || !reflect.DeepEqual(plan, want) {
			t.Fatal("v1 did not export standard v2")
		}
	}
}

func TestCanonicalChunkVerifierLimitsAndCanceledAdmission(t *testing.T) {
	raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(0, 3))
	env, stream := chunkVerifierParts(t, raw)
	p := chunkVerifierPlan(env, stream, 7)
	// Exercise actual loading/assembly/admission at the exact canonical size,
	// using a smaller internal budget so the test need not allocate 512 MiB.
	v1, ok := PlanDocChunksV1(raw)
	if !ok {
		t.Fatal("missing v1 fixture")
	}
	_, events, _ := splitCanonicalDocBytes(raw)
	v1Split := buildDocChunkPlan(ChunkFormatV1, env, [][]byte{events[0], events[1], events[2]})
	for _, plan := range []DocChunkPlan{p, v1, v1Split} {
		for _, limit := range []int{len(env) + 24, len(raw) - 1, len(raw), len(raw) + 1} {
			var v CanonicalDocVerifier
			got, err := v.verifyChunks(context.Background(), HashContent(raw), plan.Manifest, chunkVerifierLoad(plan), limit)
			if (err == nil) != (limit >= len(raw)) {
				t.Fatalf("format %s size limit %d: %v", plan.Manifest.Format, limit, err)
			}
			if err != nil && (got.Valid() || len(v.events) != 0) {
				t.Fatal("size rejection admitted proof")
			}
		}
	}
	for _, bad := range []DocChunkManifest{
		{Format: "unknown", Envelope: env},
		{Format: ChunkFormatV2, Envelope: env, Chunks: make([]ContentHash, MaxDocJobChunks+1)},
		{Format: ChunkFormatV2, Envelope: bytes.Repeat([]byte{'x'}, MaxDocJobManifestBytes+1)},
		{Format: ChunkFormatV2, Envelope: env, Chunks: []ContentHash{"bad"}},
	} {
		var v CanonicalDocVerifier
		proof, err := v.VerifyChunks(context.Background(), HashContent(raw), bad, func(context.Context, ContentHash) ([]byte, error) {
			t.Fatal("loaded invalid manifest")
			return nil, nil
		})
		if err == nil || proof.Valid() {
			t.Fatal("unbounded/invalid manifest accepted")
		}
	}
	// Discover all checkpoints and cancel at each of them, including the final
	// admission fence. No partial proof may enter the shared cache.
	checks := 0
	var probe CanonicalDocVerifier
	_, err := probe.VerifyChunks(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, HashContent(raw), p.Manifest, chunkVerifierLoad(p))
	if err != nil {
		t.Fatal(err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		var v CanonicalDocVerifier
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		observed := canonicalAdmissionContext{Context: ctx, check: func() {
			calls++
			if calls == cancelAt {
				cancel()
			}
		}}
		got, err := v.VerifyChunks(observed, HashContent(raw), p.Manifest, chunkVerifierLoad(p))
		cancel()
		if !errors.Is(err, context.Canceled) || got.Valid() || len(v.events) != 0 || len(v.order) != 0 {
			t.Fatalf("checkpoint %d admitted canceled proof: %v", cancelAt, err)
		}
	}
	for _, badStream := range [][]byte{
		bytes.Replace(stream, []byte(`"seq":2`), []byte(`"seq":0`), 1),
		bytes.Replace(stream, []byte(`"seq":2`), []byte(`"seq":2,"unknown":true`), 1),
		append(bytes.Clone(stream), ','),
	} {
		var v CanonicalDocVerifier
		seed := canonicalAdmissionDoc(t, canonicalAdmissionEvents(100, 1))
		if _, err := v.Verify(context.Background(), HashContent(seed), seed); err != nil {
			t.Fatal(err)
		}
		before, order, next := maps.Clone(v.events), slices.Clone(v.order), v.next
		bad := chunkVerifierPlan(env, badStream, 7)
		_, err := v.VerifyChunks(context.Background(), HashContent(AssembleCanonicalDoc(env, [][]byte{badStream})), bad.Manifest, chunkVerifierLoad(bad))
		if err == nil || !maps.Equal(before, v.events) || !slices.Equal(order, v.order) || next != v.next {
			t.Fatal("rejected document admitted pending proofs")
		}
	}
}

func TestCanonicalChunkVerifierConcurrentOwnedPlans(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	p := chunkVerifierPlan(env, stream, 11)
	var v CanonicalDocVerifier
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				got, err := v.VerifyChunks(context.Background(), HashContent(raw), p.Manifest, chunkVerifierLoad(p))
				if err != nil {
					t.Error(err)
					return
				}
				plan, _ := got.ChunkPlan()
				for _, body := range plan.Bodies {
					clear(body)
				}
				read, err := got.PlanReadIndex()
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := read.Build(nil); err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(got.Bytes(), raw) {
					t.Error("concurrent output mutation changed proof")
				}
			}
		}()
	}
	wg.Wait()
}

func TestSegmentedReadPlanCancellationAndSharedOwnership(t *testing.T) {
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceModels: []string{"model"}}, Events: canonicalAdmissionEvents(0, 3)}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := PlanDocChunks(raw)
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []VerifiedSessionDoc{legacy, chunked} {
		checks := 0
		plan, err := doc.PlanReadIndexContext(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }})
		if err != nil {
			t.Fatal(err)
		}
		for at := 1; at <= checks; at++ {
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			partial, err := doc.PlanReadIndexContext(canonicalAdmissionContext{Context: ctx, check: func() {
				calls++
				if calls == at {
					cancel()
				}
			}})
			cancel()
			if !errors.Is(err, context.Canceled) || partial.index.Hash != "" {
				t.Fatalf("read planning checkpoint %d did not cancel", at)
			}
		}
		// A single read plan and its source document are shared across callers.
		// Returned metadata, arrays and chunk bodies must never alias either.
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					env := plan.Envelope()
					env.SourceModels[0] = "changed"
					idx, err := plan.Build(nil)
					if err != nil {
						t.Error(err)
						return
					}
					if idx.Envelope.SourceModels[0] != "model" {
						t.Error("envelope alias")
					}
					idx.Envelope.SourceModels[0] = "changed"
					idx.Events[0].Hash = "changed"
					chunks, _ := doc.ChunkPlan()
					for _, b := range chunks.Bodies {
						clear(b)
					}
					if !bytes.Equal(doc.Bytes(), raw) {
						t.Error("shared source changed")
					}
				}
			}()
		}
		wg.Wait()
	}
}

func TestCanonicalChunkVerifierDefersBoundedCacheAdmission(t *testing.T) {
	raw := canonicalAdmissionDoc(t, canonicalAdmissionEvents(0, maxCanonicalEventProofs+3))
	input, _ := PlanDocChunks(raw)
	var v CanonicalDocVerifier
	for scan := 0; scan < 2; scan++ {
		before, order, next := maps.Clone(v.events), slices.Clone(v.order), v.next
		ctx := canonicalAdmissionContext{Context: context.Background(), check: func() {
			// Observe only bounded metadata. Comparing the entire map at every
			// event would make this regression test quadratic.
			if len(v.events) != len(before) || len(v.order) != len(order) || v.next != next {
				t.Fatal("cache admitted before whole-document success")
			}
		}}
		got, err := v.VerifyChunks(ctx, HashContent(raw), input.Manifest, chunkVerifierLoad(input))
		if err != nil || !got.Valid() {
			t.Fatal("capacity scan failed", err)
		}
		if len(v.events) != maxCanonicalEventProofs || len(v.order) != maxCanonicalEventProofs {
			t.Fatal("unbounded admission")
		}
	}
}

func FuzzCanonicalChunkVerifierOracleParity(f *testing.F) {
	raw := verifierFixture(f)
	env, stream := chunkVerifierParts(f, raw)
	f.Add(string(env), string(stream), uint16(7))
	f.Add(string(env), "", uint16(1))
	f.Fuzz(func(t *testing.T, envelope, events string, width uint16) {
		// Avoid spending the fuzz budget on expected resource-limit rejection.
		if len(envelope)+len(events) > 64<<10 {
			return
		}
		stride := max(int(width), (len(events)+MaxDocJobChunks-1)/MaxDocJobChunks, 1)
		p := chunkVerifierPlan([]byte(envelope), []byte(events), stride)
		body := AssembleCanonicalDoc([]byte(envelope), [][]byte{[]byte(events)})
		var v CanonicalDocVerifier
		var cir CIRDocument
		if json.Unmarshal(body, &cir) == nil {
			if canonical, err := CanonicalBytes(cir); err == nil {
				_, _ = v.Verify(context.Background(), HashContent(canonical), canonical)
			}
		}
		_, got := v.VerifyChunks(context.Background(), HashContent(body), p.Manifest, chunkVerifierLoad(p))
		want := canonicalVerificationOracle(body)
		if (got == nil) != (want == nil) {
			t.Fatalf("got %v; oracle %v", got, want)
		}
	})
}

func TestCanonicalChunkVerifierDepthBoundaryParity(t *testing.T) {
	env, err := canonicalJSON(CIREnvelope{CIRVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	event, err := canonicalJSON(CIREvent{Kind: EventToolCall, Input: map[string]any{"x": 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"array", "object"} {
		for nesting := 9995; nesting <= 9998; nesting++ {
			t.Run(fmt.Sprintf("%s/%d", shape, nesting), func(t *testing.T) {
				open, close := "[", "]"
				if shape == "object" {
					open, close = `{"x":`, "}"
				}
				nested := strings.Repeat(open, nesting) + "0" + strings.Repeat(close, nesting)
				stream := bytes.Replace(event, []byte(`"x":0`), []byte(`"x":`+nested), 1)
				raw := AssembleCanonicalDoc(env, [][]byte{stream})
				p := chunkVerifierPlan(env, stream, 4093)
				for _, cached := range []bool{false, true} {
					var v CanonicalDocVerifier
					if cached {
						// An isolated event may be valid even when the full wrapper
						// exceeds the limit. A cache hit must not bypass framing.
						var pending []canonicalEventProof
						if _, err := v.event(context.Background(), "2", stream, &pending); err != nil {
							t.Fatal(err)
						}
						v.remember(pending)
					}
					_, original := v.Verify(context.Background(), HashContent(raw), raw)
					_, segmented := v.VerifyChunks(context.Background(), HashContent(raw), p.Manifest, chunkVerifierLoad(p))
					oracle := canonicalVerificationOracle(raw)
					if (original == nil) != (oracle == nil) || (segmented == nil) != (oracle == nil) {
						t.Fatalf("depth=%d cached=%t: oracle=%v raw=%v chunks=%v", nesting+4, cached, oracle, original, segmented)
					}
					// Typed verification must also enforce the whole-wrapper limit.
					var typedEvent CIREvent
					if err := json.Unmarshal(stream, &typedEvent); err != nil {
						t.Fatal(err)
					}
					_, typed := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{typedEvent}}})
					if (typed == nil) != (oracle == nil) {
						t.Fatalf("typed %v differs from full oracle %v", typed, oracle)
					}
				}
			})
		}
	}
}
