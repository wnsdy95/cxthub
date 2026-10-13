package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func rootEventsFixture(test *testing.T) (ConversationManifest, map[ContentHash][]byte, CIRDocument) {
	test.Helper()
	stream := `{"blocks":[{"text":"first","type":"text"}],"kind":"message","role":"user","seq":0},` +
		`{"blocks":[{"text":"` + strings.Repeat("x", 3*ConversationManifestChunkBytes) + "\ud55c" + `\\\"","type":"text"}],"kind":"message","role":"assistant","seq":1},` +
		`{"blocks":[{"text":"last","type":"text"}],"kind":"message","role":"user","seq":2}`
	envelope := conversationTestEnvelope(test, false)
	manifest, bodies := conversationTestPlan(test, envelope, []byte(stream), 3)
	var document CIRDocument
	if err := json.Unmarshal(AssembleCanonicalDoc(envelope, [][]byte{[]byte(stream)}), &document); err != nil {
		test.Fatal(err)
	}
	return manifest, bodies, document
}

func TestRootEventsProjectionOwnsOnlySelectedBytes(test *testing.T) {
	ctx := context.Background()
	manifest, bodies, document := rootEventsFixture(test)
	hash := manifestProofHash(test, manifest)
	var verifier CanonicalDocVerifier
	hashes, err := verifier.VerifyConversationEvents(ctx, hash, manifest, manifestProofLoad(bodies), RootEventSelection{HashesOnly: true})
	if err != nil || len(hashes.events) != 0 || len(hashes.hashes) != 3 {
		test.Fatal("hash-only projection retained bodies", err)
	}
	base, err := hashes.EventHashes()
	if err != nil {
		test.Fatal(err)
	}
	for _, selection := range []RootEventSelection{
		{Offset: 0, Limit: 1}, {Offset: 0, Limit: 100}, {Offset: 1, Limit: 100}, {Offset: 2, Limit: 1},
		{Offset: 3, Limit: 1}, {Offset: -1, Limit: 1, Base: base[:1]}, {Offset: -1, Limit: 1, Base: base},
		{Offset: -1, Limit: 0, Base: []ContentHash{HashContent([]byte("different")), base[1]}},
	} {
		projection, err := verifier.VerifyConversationEvents(ctx, hash, manifest, manifestProofLoad(bodies), selection)
		if err != nil {
			test.Fatal(err)
		}
		page, err := projection.Page(ctx)
		if err != nil || !reflect.DeepEqual(page.Envelope, document.Envelope) || page.Total != 3 || len(projection.hashes) != 0 {
			test.Fatal("projection metadata changed", err)
		}
		start := selection.Offset
		inherited := 0
		for inherited < len(selection.Base) && inherited < len(base) && selection.Base[inherited] == base[inherited] {
			inherited++
		}
		if start == -1 {
			start = inherited
		}
		end := min(start+1, 3)
		if page.Offset != start || page.Inherited != inherited || !reflect.DeepEqual(page.Events, append([]CIREvent{}, document.Events[start:end]...)) {
			test.Fatal("page selection or byte limit changed")
		}
		if page.Next != -1 && page.Next != end || (page.Next == -1) != (end == 3) {
			test.Fatal("page continuation changed")
		}
		if !projection.Reference().Matches(Snapshot{ID: hash, DocHash: hash, DocIdentity: DocumentIdentityRootV1}) || projection.Reference().Matches(Snapshot{ID: hash, DocHash: hash}) {
			test.Fatal("projection identity lost")
		}
		if _, err := projection.EventHashes(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("page presented as complete hash index")
		}
	}
	buffer := make([]byte, ConversationManifestChunkBytes)
	order := slices.Clone(manifest.Chunks)
	calls := 0
	projection, err := verifier.VerifyConversationEvents(ctx, hash, manifest, func(_ context.Context, chunk ContentHash) ([]byte, error) {
		if calls == 0 {
			clear(manifest.Envelope)
			clear(manifest.Chunks)
			clear(base)
		}
		if chunk != order[calls].Hash {
			test.Fatal("chunk occurrence order changed")
		}
		calls++
		length := copy(buffer, bodies[chunk])
		return buffer[:length], nil
	}, RootEventSelection{Offset: -1, Limit: 1, Base: base[:1]})
	if err != nil || calls != len(order) {
		test.Fatal("mutable inputs changed projection", err)
	}
	clear(buffer)
	for _, body := range bodies {
		clear(body)
	}
	page, err := projection.Page(ctx)
	if err != nil || page.Inherited != 1 || !reflect.DeepEqual(page.Events, document.Events[1:2]) || !reflect.DeepEqual(page.Envelope, document.Envelope) {
		test.Fatal("selected event or envelope aliases loader memory", err)
	}
	page.Events[0].Blocks[0].Text = "mutated returned page"
	again, err := projection.Page(ctx)
	if err != nil || !reflect.DeepEqual(again.Events, document.Events[1:2]) {
		test.Fatal("returned page mutated proof", err)
	}
	if _, err := hashes.Page(ctx); !errors.Is(err, ErrIntegrity) {
		test.Fatal("hash-only proof presented as page")
	}
	if _, err := (VerifiedRootEvents{}).Page(ctx); !errors.Is(err, ErrIntegrity) {
		test.Fatal("empty proof presented as page")
	}
}

func TestRootEventsProjectionRejectsUnusedTailAndEveryOccurrence(test *testing.T) {
	for _, mode := range []string{"tail", "repeat", "length", "missing", "count-low", "count-high", "nil-loader"} {
		test.Run(mode, func(test *testing.T) {
			manifest, bodies, _ := rootEventsFixture(test)
			hash := manifestProofHash(test, manifest)
			for _, warm := range []bool{false, true} {
				var verifier CanonicalDocVerifier
				if warm {
					if _, err := verifier.VerifyConversationEvents(context.Background(), hash, manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1}); err != nil {
						test.Fatal(err)
					}
				}
				before, order, next := maps.Clone(verifier.events), slices.Clone(verifier.order), verifier.next
				candidate := manifest
				seen := map[ContentHash]bool{}
				calls := 0
				loader := func(_ context.Context, chunk ContentHash) ([]byte, error) {
					calls++
					body := bytes.Clone(bodies[chunk])
					if mode == "repeat" && seen[chunk] || mode == "tail" && calls == len(manifest.Chunks) {
						body[len(body)-1] ^= 1
					}
					if calls == len(manifest.Chunks) {
						if mode == "length" {
							body = body[:len(body)-1]
						}
						if mode == "missing" {
							return nil, ErrNotFound
						}
					}
					seen[chunk] = true
					return body, nil
				}
				switch mode {
				case "count-low":
					candidate.EventCount--
				case "count-high":
					candidate.EventCount++
				case "nil-loader":
					loader = nil
				}
				projection, err := verifier.VerifyConversationEvents(context.Background(), manifestProofHash(test, candidate), candidate, loader, RootEventSelection{Limit: 1})
				want := ErrIntegrity
				if mode == "missing" {
					want = ErrNotFound
				}
				if !errors.Is(err, want) || projection.Reference().Valid() || len(projection.events) != 0 || !maps.Equal(before, verifier.events) || !slices.Equal(order, verifier.order) || next != verifier.next {
					test.Fatalf("warm=%v rejection leaked page/cache: %v", warm, err)
				}
			}
		})
	}
}

func TestRootEventsProjectionCanonicalParity(test *testing.T) {
	raw := verifierFixture(test)
	envelope, stream := chunkVerifierParts(test, raw)
	for _, testcase := range []struct{ name, old, replacement string }{
		{"unchanged", "", ""}, {"precision", "9007199254740992", "9007199254740993"},
		{"decimal", "9007199254740992", "1.0"}, {"exponent", "9007199254740992", "1e0"},
		{"regression", `"seq":2`, `"seq":0`}, {"equal-seq", `"seq":1`, `"seq":0`},
		{"overflow", `"seq":2`, `"seq":9223372036854775808`},
		{"duplicate", `"seq":1`, `"seq":1,"seq":1`}, {"unknown", `"seq":1`, `"seq":1,"unknown":0`},
		{"utf8", "original", string([]byte{0xff})}, {"comma", "},{", "}{"},
		{"whitespace", "},{", "}, {"}, {"escape", "original", `\q`},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			candidate := bytes.Replace(stream, []byte(testcase.old), []byte(testcase.replacement), 1)
			if testcase.name != "unchanged" && bytes.Equal(candidate, stream) {
				test.Fatal("mutation missed")
			}
			manifest, bodies := conversationTestPlan(test, envelope, candidate, 3)
			hash := manifestProofHash(test, manifest)
			want := canonicalVerificationOracle(AssembleCanonicalDoc(envelope, [][]byte{candidate}))
			for _, warm := range []bool{false, true} {
				var verifier CanonicalDocVerifier
				if warm {
					if _, err := verifier.Verify(context.Background(), HashContent(raw), raw); err != nil {
						test.Fatal(err)
					}
				}
				before := maps.Clone(verifier.events)
				projection, err := verifier.VerifyConversationEvents(context.Background(), hash, manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1})
				if (err == nil) != (want == nil) || projection.Reference().Valid() != (want == nil) || err != nil && !maps.Equal(before, verifier.events) {
					test.Fatalf("canonical parity warm=%v: %v / %v", warm, err, want)
				}
			}
		})
	}
}

func TestRootEventsProjectionCancellationAndClaims(test *testing.T) {
	envelope, stream := chunkVerifierParts(test, verifierFixture(test))
	manifest, bodies := conversationTestPlan(test, envelope, stream, 3)
	hash := manifestProofHash(test, manifest)
	var verifier CanonicalDocVerifier
	for _, invalid := range []ContentHash{"invalid", HashContent([]byte("wrong root"))} {
		proof, err := verifier.VerifyConversationEvents(context.Background(), invalid, manifest, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("wrong root loaded chunks")
			return nil, nil
		}, RootEventSelection{Limit: 1})
		if !errors.Is(err, ErrIntegrity) || proof.Reference().Valid() {
			test.Fatal("wrong root accepted", err)
		}
	}
	for _, selection := range []RootEventSelection{{Offset: -2}, {Offset: 4}, {Limit: -1}, {Limit: 101}} {
		proof, err := verifier.VerifyConversationEvents(context.Background(), hash, manifest, manifestProofLoad(bodies), selection)
		if !errors.Is(err, ErrValidation) || proof.Reference().Valid() || len(verifier.events) != 0 {
			test.Fatal("invalid range admitted", err)
		}
	}
	checks := 0
	if _, err := verifier.VerifyConversationEvents(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, hash, manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1}); err != nil {
		test.Fatal(err)
	}
	for checkpoint := 1; checkpoint <= checks; checkpoint++ {
		var verifier CanonicalDocVerifier
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		observed := canonicalAdmissionContext{Context: ctx, check: func() {
			calls++
			if calls == checkpoint {
				cancel()
			}
		}}
		proof, err := verifier.VerifyConversationEvents(observed, hash, manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1})
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Reference().Valid() || len(verifier.events) != 0 {
			test.Fatalf("cancellation checkpoint=%d admitted page/cache: %v", checkpoint, err)
		}
	}
}

func TestRootEventsProjectionCachedVersionAndDepthFences(test *testing.T) {
	ctx := context.Background()
	envelope := conversationTestEnvelope(test, false)
	stream := []byte(`{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":0}`)
	manifest, bodies := conversationTestPlan(test, envelope, stream, 1)
	var verifier CanonicalDocVerifier
	if _, err := verifier.VerifyConversationEvents(ctx, manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1}); err != nil {
		test.Fatal(err)
	}
	oldEnvelope := bytes.Replace(envelope, []byte(`"cir_version":"2"`), []byte(`"cir_version":"1"`), 1)
	manifest, bodies = conversationTestPlan(test, oldEnvelope, stream, 1)
	proof, err := verifier.VerifyConversationEvents(ctx, manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1})
	if !errors.Is(err, ErrUnsupportedCIRVersion) || errors.Is(err, ErrIntegrity) || proof.Reference().Valid() {
		test.Fatal("warm cache bypassed version fence", err)
	}
	template, err := canonicalJSON(CIREvent{Kind: EventToolCall, Input: map[string]any{"x": 0}})
	if err != nil {
		test.Fatal(err)
	}
	for _, nesting := range []int{9996, 9997} {
		event := bytes.Replace(template, []byte(`"x":0`), []byte(`"x":`+strings.Repeat("[", nesting)+"0"+strings.Repeat("]", nesting)), 1)
		manifest, bodies := conversationTestPlan(test, envelope, event, 1)
		var pending []canonicalEventProof
		if _, err := verifier.event(ctx, "2", event, &pending); err != nil {
			test.Fatal(err)
		}
		verifier.remember(pending)
		proof, err := verifier.VerifyConversationEvents(ctx, manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies), RootEventSelection{Limit: 1})
		if (err == nil) != (nesting == 9996) || proof.Reference().Valid() != (err == nil) {
			test.Fatal("cached event bypassed document depth fence", err)
		}
	}
}
