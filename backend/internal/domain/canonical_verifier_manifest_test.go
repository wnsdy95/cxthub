package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func manifestProofLoad(bodies map[ContentHash][]byte) func(context.Context, ContentHash) ([]byte, error) {
	return func(_ context.Context, h ContentHash) ([]byte, error) { return bodies[h], nil }
}

func manifestProofHash(t testing.TB, m ConversationManifest) ContentHash {
	t.Helper()
	h, err := ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestConversationManifestDocIdentityReadPlanAndLegacyFences(t *testing.T) {
	fixture := verifierFixture(t)
	env, stream := chunkVerifierParts(t, fixture)
	for _, tc := range []struct {
		name        string
		env, stream []byte
		count       int64
	}{
		{"empty", env, nil, 0},
		{"nested-replacement", env, stream, 3},
		{"typed-precision", conversationTestEnvelope(t, true), []byte(conversationVectorStream), 2},
		{"explicit-v1", bytes.Replace(conversationTestEnvelope(t, false), []byte(`"cir_version":"2"`), []byte(`"cir_version":"1"`), 1), []byte(conversationVectorStream), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, bodies := conversationTestPlan(t, tc.env, tc.stream, tc.count)
			root := manifestProofHash(t, m)
			raw := AssembleCanonicalDoc(tc.env, [][]byte{tc.stream})
			var v CanonicalDocVerifier
			legacy, err := v.Verify(context.Background(), HashContent(raw), raw)
			if err != nil {
				t.Fatal(err)
			}
			wantIndex, err := legacy.ReadIndex()
			if err != nil {
				t.Fatal(err)
			}
			wantIndex.Hash = root
			for _, r := range [][2]int{{0, len(tc.stream)}, {len(tc.stream), 0}} {
				part, err := legacy.EventStreamRange(context.Background(), r[0], r[1])
				if err != nil || !bytes.Equal(part, tc.stream[r[0]:r[0]+r[1]]) {
					t.Fatal("legacy event range drift", err)
				}
			}
			for _, warm := range []bool{false, true} {
				if !warm {
					v = CanonicalDocVerifier{}
				}
				got, err := v.VerifyConversationManifestDoc(context.Background(), root, m, manifestProofLoad(bodies))
				if err != nil || !got.Valid() || got.Hash() != root || !bytes.Equal(got.Bytes(), raw) || HashContent(got.Bytes()) == root {
					t.Fatalf("root/CIR identity warm=%v: %v", warm, err)
				}
				if got.canonical != "" || got.chunks == nil || len(got.chunks.events) != int(tc.count) {
					t.Fatal("proof must retain stream and complete spans")
				}
				ref := DocumentRef{Hash: root, Identity: DocumentIdentityRootV1}
				if got.DocumentRef() != ref || got.Reference().DocumentRef() != ref {
					t.Fatal("identity lost from proof")
				}
				for _, snap := range []Snapshot{
					{ID: root, DocHash: root, DocIdentity: DocumentIdentityRootV1},
					{ID: root, DocHash: root},
					{ID: HashContent(raw), DocHash: root, DocIdentity: DocumentIdentityRootV1},
					{ID: root, DocHash: HashContent(raw), DocIdentity: DocumentIdentityRootV1},
					{ID: root, DocHash: root, DocIdentity: "future"},
				} {
					want := snap.ID == root && snap.DocHash == root && snap.DocIdentity == DocumentIdentityRootV1
					if got.Reference().Matches(snap) != want {
						t.Fatal("scheme/hash/S-ID proof mismatch")
					}
				}
				owned, ok := got.ConversationManifest()
				if !ok || !reflect.DeepEqual(owned, m) {
					t.Fatal("root manifest changed")
				}
				if _, ok := got.ChunkPlan(); ok {
					t.Fatal("root exported lossy legacy descriptor")
				}
				part, err := got.EventStreamRange(context.Background(), len(tc.stream), 0)
				if err != nil || len(part) != 0 {
					t.Fatal("end range including empty root failed", err)
				}
				idx, err := got.ReadIndex()
				if err != nil || !reflect.DeepEqual(idx, wantIndex) {
					t.Fatal("root read index differs", err)
				}
				plan, err := got.PlanReadIndexContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				var events []DocEventIndex
				for _, b := range plan.Blocks() {
					built, err := b.Build(nil)
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range built.Events {
						event.Index += b.FirstEvent
						event.Offset += b.Offset
						events = append(events, event)
					}
				}
				if !slices.Equal(events, wantIndex.Events) {
					t.Fatal("block spans or projected text drifted")
				}
			}
			var cir CIRDocument
			if err := json.Unmarshal(raw, &cir); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifySessionDoc(SessionDoc{Hash: root, Identity: DocumentIdentityRootV1, CIR: cir}); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
				t.Fatal("legacy writer fence widened", err)
			}
			if _, err := VerifyStoredDocBytes(root, raw); err == nil {
				t.Fatal("root relabeled as legacy bytes")
			}
			if _, err := v.Verify(context.Background(), root, raw); err == nil {
				t.Fatal("root accepted as whole-CIR digest")
			}
			if _, ok := legacy.ConversationManifest(); ok || legacy.DocumentRef().Identity != DocumentIdentityLegacy {
				t.Fatal("legacy proof relabeled")
			}
			if legacy.Reference().Matches(Snapshot{ID: legacy.Hash(), DocHash: legacy.Hash(), DocIdentity: DocumentIdentityRootV1}) {
				t.Fatal("legacy reference matched root scheme")
			}
		})
	}
}

func TestConversationManifestDocOwnedSeamsRepeatedLoadsAndRanges(t *testing.T) {
	for _, seam := range []string{"\ud55c🙂", "\\\"", "\n\t"} {
		t.Run(fmt.Sprintf("%q", seam), func(t *testing.T) {
			cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "MARKER"}}}}}
			initial, _ := CanonicalBytes(cir)
			_, initialStream := chunkVerifierParts(t, initial)
			textOffset := bytes.Index(initialStream, []byte("MARKER"))
			cir.Events[0].Blocks[0].Text = strings.Repeat("x", ConversationManifestChunkBytes-textOffset-1) + seam + strings.Repeat("x", 3*ConversationManifestChunkBytes)
			for i := 1; i < ReadIndexBlockEvents+2; i++ {
				cir.Events = append(cir.Events, CIREvent{Kind: EventTurn, Role: RoleAssistant, Seq: i})
			}
			raw, err := CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			env, stream := chunkVerifierParts(t, raw)
			m, bodies := conversationTestPlan(t, env, stream, int64(len(cir.Events)))
			if len(m.Chunks) <= len(bodies) {
				t.Fatal("fixture must repeat a chunk hash")
			}
			root := manifestProofHash(t, m)
			before, _ := CanonicalConversationManifest(m)
			var v CanonicalDocVerifier
			var scratch []byte
			var loads []ContentHash
			got, err := v.VerifyConversationManifestDoc(context.Background(), root, m, func(_ context.Context, h ContentHash) ([]byte, error) {
				loads = append(loads, h)
				scratch = append(scratch[:0], bodies[h]...)
				m.Envelope[0] = '!'
				m.Chunks[0].Bytes++
				return scratch, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			clear(scratch)
			owned, ok := got.ConversationManifest()
			if !ok {
				t.Fatal("no root manifest")
			}
			wire, _ := CanonicalConversationManifest(owned)
			if !bytes.Equal(wire, before) || len(loads) != len(owned.Chunks) {
				t.Fatal("manifest mutated or chunk occurrence skipped")
			}
			for i, h := range loads {
				if h != owned.Chunks[i].Hash {
					t.Fatal("chunk order changed")
				}
			}
			owned.Envelope[0] = '!'
			owned.Chunks[0].Hash = "bad"
			copy := got.Bytes()
			clear(copy)
			for _, r := range [][2]int{{0, len(stream)}, {0, 0}, {len(stream), 0}, {ConversationManifestChunkBytes - 2, 11}, {ConversationManifestChunkBytes - 3, ConversationManifestChunkBytes + 13}} {
				part, err := got.EventStreamRange(context.Background(), r[0], r[1])
				if err != nil || !bytes.Equal(part, stream[r[0]:r[0]+r[1]]) {
					t.Fatal("range changed across seam", err)
				}
				clear(part)
			}
			for _, r := range [][2]int{{-1, 1}, {0, -1}, {len(stream) + 1, 0}, {len(stream), 1}, {1, math.MaxInt}} {
				if part, err := got.EventStreamRange(context.Background(), r[0], r[1]); !errors.Is(err, ErrIntegrity) || part != nil {
					t.Fatal("invalid range returned bytes", r, err)
				}
			}
			legacy, err := v.Verify(context.Background(), HashContent(raw), raw)
			if err != nil {
				t.Fatal(err)
			}
			wantIndex, _ := legacy.ReadIndex()
			wantIndex.Hash = root
			idx, err := got.ReadIndex()
			if err != nil || !reflect.DeepEqual(idx, wantIndex) {
				t.Fatal("giant-event read index drift", err)
			}
			for _, event := range idx.Events {
				part, err := got.EventStreamRange(context.Background(), event.Offset, event.Length)
				if err != nil || HashContent(part) != event.Hash {
					t.Fatal("indexed range/hash mismatch", err)
				}
			}
			valid, _ := got.ConversationManifest()
			calls := 0
			warm, err := v.VerifyConversationManifestDoc(context.Background(), root, valid, func(_ context.Context, h ContentHash) ([]byte, error) { calls++; return bodies[h], nil })
			if err != nil || !warm.Valid() || calls != len(valid.Chunks) {
				t.Fatal("warm cache skipped current occurrence", err)
			}
			// A fresh request must reject corruption on the SECOND occurrence
			// of a repeated hash, despite its earlier valid load/cache entry.
			seen := map[ContentHash]bool{}
			bad, err := v.VerifyConversationManifestDoc(context.Background(), root, valid, func(_ context.Context, h ContentHash) ([]byte, error) {
				body := bytes.Clone(bodies[h])
				if seen[h] {
					body[0] ^= 1
				}
				seen[h] = true
				return body, nil
			})
			if !errors.Is(err, ErrIntegrity) || bad.Valid() {
				t.Fatal("repeated current hash trusted", err)
			}
			if !bytes.Equal(got.Bytes(), raw) {
				t.Fatal("proof aliased loader, output or new request")
			}
		})
	}
}

func TestConversationManifestDocRejectsClaimsBeforeLoadingAndAdmission(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	valid, bodies := conversationTestPlan(t, env, stream, 3)
	root := manifestProofHash(t, valid)
	for name, change := range map[string]func(*ConversationManifest){
		"scheme":        func(m *ConversationManifest) { m.Identity = "future" },
		"version":       func(m *ConversationManifest) { m.Version++ },
		"format":        func(m *ConversationManifest) { m.ChunkFormat = ChunkFormatV1 },
		"nil-chunks":    func(m *ConversationManifest) { m.Chunks = nil },
		"envelope-size": func(m *ConversationManifest) { m.Envelope = bytes.Repeat([]byte{'x'}, MaxConversationManifestBytes+1) },
		"chunks-limit": func(m *ConversationManifest) {
			m.Chunks = make([]ConversationManifestChunk, MaxConversationManifestChunks+1)
		},
		"chunk-hash":         func(m *ConversationManifest) { m.Chunks[0].Hash = "bad" },
		"partition":          func(m *ConversationManifest) { m.Chunks = append(m.Chunks, m.Chunks[0]); m.StreamBytes *= 2 },
		"total":              func(m *ConversationManifest) { m.StreamBytes++ },
		"document-limit":     func(m *ConversationManifest) { m.StreamBytes = MaxConversationDocumentBytes },
		"overflow-length":    func(m *ConversationManifest) { m.Chunks[0].Bytes = math.MaxInt64 },
		"overflow-count":     func(m *ConversationManifest) { m.EventCount = math.MaxInt64 },
		"zero-forged-count":  func(m *ConversationManifest) { m.EventCount = 0 },
		"unknown-envelope":   func(m *ConversationManifest) { m.Envelope = []byte(`{"cir_version":"2","unknown":0}`) },
		"duplicate-envelope": func(m *ConversationManifest) { m.Envelope = []byte(`{"cir_version":"2","cir_version":"2"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			m := valid
			m.Chunks = slices.Clone(valid.Chunks)
			change(&m)
			var v CanonicalDocVerifier
			got, err := v.VerifyConversationManifestDoc(context.Background(), root, m, func(context.Context, ContentHash) ([]byte, error) {
				t.Fatal("loaded invalid metadata")
				return nil, nil
			})
			if err == nil || got.Valid() || len(v.events) != 0 {
				t.Fatal("invalid metadata admitted")
			}
		})
	}
	for _, want := range []ContentHash{"bad", HashContent(raw), HashContent([]byte("other"))} {
		var v CanonicalDocVerifier
		if got, err := v.VerifyConversationManifestDoc(context.Background(), want, valid, func(context.Context, ContentHash) ([]byte, error) { t.Fatal("loaded wrong root"); return nil, nil }); err == nil || got.Valid() {
			t.Fatal("wrong root accepted")
		}
	}
	marker := errors.New("synthetic loader failure")
	for _, mode := range []string{"nil-loader", "missing", "wrong-length", "wrong-hash", "loader-error", "count-low", "count-high"} {
		t.Run(mode, func(t *testing.T) {
			for _, warm := range []bool{false, true} {
				var v CanonicalDocVerifier
				if warm {
					if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
						t.Fatal(err)
					}
				}
				before, order, next := maps.Clone(v.events), slices.Clone(v.order), v.next
				m := valid
				load := manifestProofLoad(bodies)
				switch mode {
				case "nil-loader":
					load = nil
				case "missing":
					load = func(context.Context, ContentHash) ([]byte, error) { return nil, nil }
				case "wrong-length":
					load = func(_ context.Context, h ContentHash) ([]byte, error) { return bodies[h][:len(bodies[h])-1], nil }
				case "wrong-hash":
					load = func(_ context.Context, h ContentHash) ([]byte, error) {
						return bytes.Repeat([]byte{'x'}, len(bodies[h])), nil
					}
				case "loader-error":
					load = func(context.Context, ContentHash) ([]byte, error) { return nil, marker }
				case "count-low":
					m.EventCount--
				case "count-high":
					m.EventCount++
				}
				got, err := v.VerifyConversationManifestDoc(context.Background(), manifestProofHash(t, m), m, load)
				if err == nil || got.Valid() || !maps.Equal(before, v.events) || !slices.Equal(order, v.order) || next != v.next {
					t.Fatal("rejection admitted proof/cache", err)
				}
				if mode == "loader-error" && !errors.Is(err, marker) {
					t.Fatal("loader cause lost")
				}
			}
		})
	}
}

func TestConversationManifestDocSemanticOracleParity(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	for _, tc := range []struct{ name, old, new string }{
		{"unchanged", "", ""},
		{"precision", "9007199254740992", "9007199254740993"},
		{"decimal", "9007199254740992", "1.0"},
		{"exponent", "9007199254740992", "1e0"},
		{"negative-seq", `"seq":1`, `"seq":-1`},
		{"regression", `"seq":2`, `"seq":0`},
		{"equal-seq", `"seq":1`, `"seq":0`},
		{"integer-overflow", `"seq":2`, `"seq":9223372036854775808`},
		{"duplicate", `"seq":1`, `"seq":1,"seq":1`},
		{"unknown", `"seq":1`, `"seq":1,"unknown":0`},
		{"nested-duplicate", `"n":9007199254740992`, `"n":0,"n":9007199254740992`},
		{"invalid-utf8", "original", string([]byte{0xff})},
		{"missing-comma", "},{", "}{"},
		{"extra-comma", "},{", "},,{"},
		{"whitespace", "},{", "}, {"},
		{"invalid-escape", "original", `\q`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := bytes.Replace(stream, []byte(tc.old), []byte(tc.new), 1)
			if tc.name != "unchanged" && bytes.Equal(stream, candidate) {
				t.Fatal("mutation missed")
			}
			m, bodies := conversationTestPlan(t, env, candidate, 3)
			root := manifestProofHash(t, m)
			want := conversationTestVerify(m, bodies)
			full := AssembleCanonicalDoc(env, [][]byte{candidate})
			oracle := canonicalVerificationOracle(full)
			if (oracle == nil) != (want == nil) {
				t.Fatal("standalone oracle drift", oracle, want)
			}
			for _, warm := range []bool{false, true} {
				var v CanonicalDocVerifier
				if warm {
					if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
						t.Fatal(err)
					}
				}
				before := maps.Clone(v.events)
				got, err := v.VerifyConversationManifestDoc(context.Background(), root, m, manifestProofLoad(bodies))
				if (err == nil) != (want == nil) || got.Valid() != (err == nil) {
					t.Fatalf("warm=%v root=%v standalone=%v", warm, err, want)
				}
				if err != nil && !maps.Equal(before, v.events) {
					t.Fatal("invalid semantic events admitted")
				}
			}
		})
	}
	// A cached isolated event must not bypass the two outer document levels.
	template, err := canonicalJSON(CIREvent{Kind: EventToolCall, Input: map[string]any{"x": 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, nesting := range []int{9996, 9997} {
		t.Run(fmt.Sprintf("depth/%d", nesting+4), func(t *testing.T) {
			event := bytes.Replace(template, []byte(`"x":0`), []byte(`"x":`+strings.Repeat("[", nesting)+"0"+strings.Repeat("]", nesting)), 1)
			m, bodies := conversationTestPlan(t, env, event, 1)
			var v CanonicalDocVerifier
			var pending []canonicalEventProof
			if _, err := v.event(context.Background(), "2", event, &pending); err != nil {
				t.Fatal(err)
			}
			v.remember(pending)
			_, err := v.VerifyConversationManifestDoc(context.Background(), manifestProofHash(t, m), m, manifestProofLoad(bodies))
			want := conversationTestVerify(m, bodies)
			if (err == nil) != (want == nil) || (err == nil) != (nesting == 9996) {
				t.Fatal("depth fence drift", err, want)
			}
		})
	}
}

func TestConversationManifestDocCancellationAndConcurrentUse(t *testing.T) {
	raw := verifierFixture(t)
	env, stream := chunkVerifierParts(t, raw)
	m, bodies := conversationTestPlan(t, env, stream, 3)
	root := manifestProofHash(t, m)
	load := manifestProofLoad(bodies)
	for _, warm := range []bool{false, true} {
		seed := func(v *CanonicalDocVerifier) {
			if warm {
				if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
					t.Fatal(err)
				}
			}
		}
		var probe CanonicalDocVerifier
		seed(&probe)
		checks := 0
		if _, err := probe.VerifyConversationManifestDoc(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, root, m, load); err != nil {
			t.Fatal(err)
		}
		for at := 1; at <= checks; at++ {
			var v CanonicalDocVerifier
			seed(&v)
			before, order, next := maps.Clone(v.events), slices.Clone(v.order), v.next
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			observed := canonicalAdmissionContext{Context: ctx, check: func() {
				calls++
				if calls == at {
					cancel()
				}
			}}
			got, err := v.VerifyConversationManifestDoc(observed, root, m, load)
			cancel()
			if !errors.Is(err, context.Canceled) || got.Valid() || !maps.Equal(before, v.events) || !slices.Equal(order, v.order) || next != v.next {
				t.Fatalf("warm=%v checkpoint=%d admitted cancellation: %v", warm, at, err)
			}
		}
	}
	var v CanonicalDocVerifier
	proof, err := v.VerifyConversationManifestDoc(context.Background(), root, m, load)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := proof.EventStreamRange(ctx, 0, len(stream)); return err },
		func(ctx context.Context) error { _, err := proof.PlanReadIndexContext(ctx); return err },
	} {
		checks := 0
		if err := read(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}); err != nil {
			t.Fatal(err)
		}
		for at := 1; at <= checks; at++ {
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			err := read(canonicalAdmissionContext{Context: ctx, check: func() {
				calls++
				if calls == at {
					cancel()
				}
			}})
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatal("read did not cancel at checkpoint", at, err)
			}
		}
	}
	if got, err := (VerifiedSessionDoc{}).EventStreamRange(context.Background(), 0, 0); !errors.Is(err, ErrIntegrity) || got != nil {
		t.Fatal("invalid proof returned a range")
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				got, err := v.VerifyConversationManifestDoc(context.Background(), root, m, load)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := v.Verify(context.Background(), HashContent(raw), raw); err != nil {
					t.Error(err)
					return
				}
				owned, _ := got.ConversationManifest()
				owned.Envelope[0] = '!'
				owned.Chunks[0].Hash = "bad"
				part, err := proof.EventStreamRange(context.Background(), 0, len(stream))
				if err != nil {
					t.Error(err)
					return
				}
				clear(part)
				if !bytes.Equal(got.Bytes(), raw) || !bytes.Equal(proof.Bytes(), raw) {
					t.Error("concurrent mutation changed proof")
				}
			}
		}()
	}
	wg.Wait()
}
