package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func docReadProjectionOracle(test testing.TB, raw []byte, hash ContentHash) DocReadIndex {
	test.Helper()
	envelope, bodies, err := splitCanonicalDocBytes(raw)
	if err != nil {
		test.Fatal(err)
	}
	index := DocReadIndex{Version: 1, Hash: hash, Events: make([]DocEventIndex, 0, len(bodies))}
	if err := json.Unmarshal(envelope, &index.Envelope); err != nil {
		test.Fatal(err)
	}
	offset := 0
	for ordinal, body := range bodies {
		var event CIREvent
		if err := json.Unmarshal(body, &event); err != nil {
			test.Fatal(err)
		}
		index.Events = append(index.Events, DocEventIndex{
			Index: ordinal, Offset: offset, Length: len(body), Hash: HashContent(body),
			Seq: event.Seq, Role: string(event.Role), Text: SearchableEventText(event),
		})
		offset += len(body) + 1
	}
	return index
}

func docReadProjectionPlan(test testing.TB, raw []byte) DocReadPlan {
	test.Helper()
	var verifier CanonicalDocVerifier
	doc, err := verifier.Verify(context.Background(), HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := doc.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	return plan
}

type docReadProjectionMode struct {
	name  string
	build func(context.Context) (DocReadIndex, error)
	cache map[ContentHash]bool
	only  bool
}

func docReadProjectionModes(plan DocReadPlan) []docReadProjectionMode {
	known := make(map[ContentHash]bool, len(plan.EventHashes()))
	for _, hash := range plan.EventHashes() {
		known[hash] = true
	}
	return []docReadProjectionMode{
		{name: "search", build: func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, nil) }},
		{name: "reused", build: func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, known) }, cache: known},
		{name: "metadata", build: plan.BuildMetadataContext, only: true},
	}
}

func docReadProjectionExpected(oracle DocReadIndex, mode docReadProjectionMode) DocReadIndex {
	want := oracle
	want.Events = append([]DocEventIndex{}, oracle.Events...)
	for ordinal := range want.Events {
		if mode.only || mode.cache[want.Events[ordinal].Hash] {
			want.Events[ordinal].Text = ""
		}
	}
	return want
}

func docReadProjectionCheck(test testing.TB, mode docReadProjectionMode, oracle DocReadIndex) {
	test.Helper()
	got, err := mode.build(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if want := docReadProjectionExpected(oracle, mode); !reflect.DeepEqual(got, want) {
		test.Fatalf("%s projection differs from full CIREvent oracle: got=%+v want=%+v", mode.name, got, want)
	}
}

func BenchmarkDocReadSearchProjection(benchmark *testing.B) {
	for _, count := range []int{8192, 65536, 262144} {
		for _, object := range []bool{false, true} {
			benchmark.Run(fmt.Sprintf("items=%d/object=%t", count, object), func(benchmark *testing.B) {
				raw := canonicalStructuredPayloadFixture(benchmark, count, object)
				plan := docReadProjectionPlan(benchmark, raw)
				oracle := docReadProjectionOracle(benchmark, raw, HashContent(raw))
				for _, mode := range docReadProjectionModes(plan) {
					docReadProjectionCheck(benchmark, mode, oracle)
					benchmark.Run(mode.name, func(benchmark *testing.B) {
						benchmark.ReportAllocs()
						benchmark.SetBytes(int64(len(raw)))
						runtime.GC()
						runtime.GC()
						benchmark.ResetTimer()
						for iteration := 0; iteration < benchmark.N; iteration++ {
							index, err := mode.build(context.Background())
							if err != nil {
								benchmark.Fatal(err)
							}
							runtime.KeepAlive(index)
						}
						benchmark.StopTimer()
						docReadProjectionCheck(benchmark, mode, docReadProjectionOracle(benchmark, raw, HashContent(raw)))
					})
				}
			})
		}
	}
}

func TestDocReadSearchProjectionQualification(test *testing.T) {
	if os.Getenv("CXT_DOC_READ_PROJECTION_QUALIFICATION") != "1" {
		test.Skip("opt-in verified-plan search projection cost measurement")
	}
	for _, count := range []int{65536, 262144} {
		for _, object := range []bool{false, true} {
			test.Run(fmt.Sprintf("items=%d/object=%t", count, object), func(test *testing.T) {
				raw := canonicalStructuredPayloadFixture(test, count, object)
				plan := docReadProjectionPlan(test, raw)
				oracle := docReadProjectionOracle(test, raw, HashContent(raw))
				for _, mode := range docReadProjectionModes(plan) {
					test.Run(mode.name, func(test *testing.T) {
						docReadProjectionCheck(test, mode, oracle)
						runtime.GC()
						runtime.GC()
						var before runtime.MemStats
						runtime.ReadMemStats(&before)
						var peak atomic.Uint64
						peak.Store(before.HeapAlloc)
						observe := func() {
							var stats runtime.MemStats
							runtime.ReadMemStats(&stats)
							for current := peak.Load(); stats.HeapAlloc > current; current = peak.Load() {
								if peak.CompareAndSwap(current, stats.HeapAlloc) {
									break
								}
							}
						}
						stop, joined, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
						go func() {
							defer close(joined)
							observe()
							close(ready)
							ticker := time.NewTicker(time.Millisecond)
							defer ticker.Stop()
							for {
								select {
								case <-stop:
									return
								case <-ticker.C:
									observe()
								}
							}
						}()
						<-ready
						started := time.Now()
						index, err := mode.build(context.Background())
						elapsed := time.Since(started)
						observe()
						close(stop)
						<-joined
						var after runtime.MemStats
						runtime.ReadMemStats(&after)
						if err != nil || !reflect.DeepEqual(index, docReadProjectionExpected(oracle, mode)) {
							test.Fatalf("measured projection differs from oracle: %v", err)
						}
						test.Logf("items=%d object=%t mode=%s source_bytes=%d elapsed_ns=%d allocated_bytes=%d allocations=%d base_heap=%d observed_peak_heap=%d sample_interval_ns=%d", count, object, mode.name, len(raw), elapsed.Nanoseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, before.HeapAlloc, peak.Load(), time.Millisecond.Nanoseconds())
						runtime.KeepAlive(plan)
						runtime.KeepAlive(raw)
						runtime.KeepAlive(index)
					})
				}
			})
		}
	}
}

func docReadProjectionSemanticFixture(test testing.TB) []byte {
	test.Helper()
	shadow := map[string]any{
		"kind": "message", "seq": 999, "role": "nested-only", "redacted_summary": "nested summary",
		"blocks":      []any{map[string]any{"type": "text", "text": "nested block"}, nil},
		"empty_array": []any{}, "empty_object": map[string]any{}, "null": nil,
	}
	repeated := CIREvent{Kind: EventMessage, Seq: 11, Role: Role("custom 한글🙂\t\"\\" + fmt.Sprintf("%4096s", "")), Blocks: []ContentBlock{
		{Type: "text", Text: ""}, {Type: "image", Text: "not searchable"},
		{Type: "text", Text: "first\nline\x00<&>\u2028\u2029"}, {Type: "Text", Text: "case-sensitive"},
		{Type: "text", Text: " \t "}, {Type: "", Text: "not text"}, {Type: "text", Text: "last 🙂"},
	}}
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"original"}}, Events: []CIREvent{
		repeated,
		{Kind: EventTurn, Seq: 0, Role: ""},
		{Kind: EventMessage, Seq: 1, Role: RoleSystem},
		{Kind: EventMessage, Seq: 2, Role: RoleDeveloper, Blocks: []ContentBlock{}},
		{Kind: EventMessage, Seq: 3, Role: RoleAssistant, AgentMessage: true, AgentAuthor: "agent-only", AgentRecipient: "recipient-only", CompactSummary: true,
			Blocks: []ContentBlock{{Type: "text", Text: "visible agent summary"}}, Locked: &LockedBlob{Provider: "codex", Scheme: "encrypted_content", Blob: "opaque-only"}},
		{Kind: EventToolCall, Seq: 4, CallID: "call", ToolName: "tool", Input: shadow},
		{Kind: EventToolResult, Seq: 5, CallID: "call", Output: []any{shadow, nil, false, "", []any{}, map[string]any{}}},
		{Kind: EventToolCall, Seq: 6, CallID: "empty", ToolName: "empty"},
		{Kind: EventToolResult, Seq: 7, CallID: "empty"},
		{Kind: EventReasoning, Seq: 8, Locked: &LockedBlob{Provider: "codex", Scheme: "encrypted_content", Blob: "opaque-only"}},
		{Kind: EventReasoning, Seq: 9, RedactedSummary: " exact summary\n한글🙂\t ", Locked: &LockedBlob{Provider: "claude", Scheme: "signature", Blob: "not searchable"}},
		{Kind: EventCompaction, Seq: 10, ReplacementComplete: true, Replacement: []CIREvent{
			{Kind: EventMessage, Seq: 999, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "replacement must stay hidden"}}},
			{Kind: EventCompaction, Seq: 1, Replacement: []CIREvent{{Kind: EventReasoning, Seq: 0, RedactedSummary: "nested hidden summary"}}},
		}},
		repeated,
		{Kind: EventCompaction, Seq: 12, Replacement: []CIREvent{}},
		{Kind: EventCompaction, Seq: 13, Locked: &LockedBlob{Provider: "codex", Scheme: "encrypted_content", Blob: "locked replacement"}},
		{Kind: EventTurn, Seq: 9007199254740993, Role: RoleUser},
	}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docReadProjectionSemanticPlans(test testing.TB, raw []byte) map[string]DocReadPlan {
	test.Helper()
	envelope, stream := chunkVerifierParts(test, raw)
	input := chunkVerifierPlan(envelope, stream, ChunkTarget)
	plans := map[string]DocReadPlan{"monolithic": docReadProjectionPlan(test, raw)}
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input))
	if err != nil {
		test.Fatal(err)
	}
	plans["chunked"], err = chunked.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	spooled, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
	if err != nil {
		test.Fatal(err)
	}
	plans["streamed"], err = spooled.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	_, events, err := splitCanonicalDocBytes(raw)
	if err != nil {
		test.Fatal(err)
	}
	manifest, bodies := conversationTestPlan(test, envelope, stream, int64(len(events)))
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	plans["root"], err = root.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	return plans
}

func TestDocReadSearchProjectionFullTypedOracle(test *testing.T) {
	for _, fixture := range []struct {
		name string
		raw  []byte
	}{
		{"all-kinds", docReadProjectionSemanticFixture(test)},
		{"many-small-object-values", canonicalStructuredPayloadFixture(test, 8192, true)},
		{"many-small-array-values", canonicalStructuredPayloadFixture(test, 8192, false)},
		{"empty", giantCanonicalDocument(test, "")},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for source, plan := range docReadProjectionSemanticPlans(test, fixture.raw) {
				test.Run(source, func(test *testing.T) {
					oracle := docReadProjectionOracle(test, fixture.raw, plan.index.Hash)
					mixed := map[ContentHash]bool{HashContent([]byte("not in this document")): true}
					for ordinal, hash := range plan.EventHashes() {
						mixed[hash] = ordinal%2 == 0
					}
					modes := append(docReadProjectionModes(plan), docReadProjectionMode{
						name: "mixed-reuse", cache: mixed,
						build: func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, mixed) },
					})
					for _, mode := range modes {
						test.Run(mode.name, func(test *testing.T) {
							docReadProjectionCheck(test, mode, oracle)
							if mode.only {
								return
							}
							entries := []DocEventIndex{}
							for _, block := range plan.Blocks() {
								index, err := block.BuildContext(context.Background(), mode.cache)
								if err != nil {
									test.Fatal(err)
								}
								for _, entry := range index.Events {
									entry.Index += block.FirstEvent
									entry.Offset += block.Offset
									entries = append(entries, entry)
								}
							}
							if !reflect.DeepEqual(entries, docReadProjectionExpected(oracle, mode).Events) {
								test.Fatal("block projection differs from full typed oracle")
							}
						})
					}
					got, err := plan.BuildContext(context.Background(), nil)
					if err != nil {
						test.Fatal(err)
					}
					if len(got.Events) > 0 {
						got.Events[0] = DocEventIndex{Seq: -1, Role: "caller mutation", Text: "caller mutation"}
					}
					if len(got.Envelope.SourceModels) > 0 {
						got.Envelope.SourceModels[0] = "caller mutation"
					}
					docReadProjectionCheck(test, modes[0], oracle)
				})
			}
		})
	}
}

func TestDocReadSearchProjectionStreamCorruptionAndAliasing(test *testing.T) {
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 0, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "first visible event"}}},
		{Kind: EventToolResult, Seq: 1, CallID: "tool", Output: map[string]any{"payload": "opaque-only-sentinel"}},
	}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	envelope, stream := chunkVerifierParts(test, raw)
	input := chunkVerifierPlan(envelope, stream, ChunkTarget)
	spool := &legacyTestStream{}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), spool)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := proof.PlanReadIndexContext(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	oracle := docReadProjectionOracle(test, raw, HashContent(raw))
	for _, mode := range docReadProjectionModes(plan) {
		test.Run(mode.name, func(test *testing.T) {
			owned, err := mode.build(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			clear(spool.body)
			if !reflect.DeepEqual(owned, docReadProjectionExpected(oracle, mode)) {
				test.Fatal("successful projection aliases mutable source bytes")
			}
			copy(spool.body, stream)
			marker := "opaque-only-sentinel"
			changed := false
			for offset := 0; offset+len(marker) <= len(spool.body); offset++ {
				if string(spool.body[offset:offset+len(marker)]) == marker {
					spool.body[offset] = 'X'
					changed = true
					break
				}
			}
			if !changed {
				test.Fatal("missing corruption marker")
			}
			got, err := mode.build(context.Background())
			if err != ErrIntegrity || !reflect.DeepEqual(got, DocReadIndex{}) {
				test.Fatalf("ignored payload corruption returned a partial or trusted projection: %v", err)
			}
			copy(spool.body, stream)
			for _, delta := range []int{-1, 1} {
				spool.readTransform = func(body []byte) []byte {
					if delta < 0 {
						return body[:len(body)-1]
					}
					return append(append([]byte(nil), body...), ' ')
				}
				got, err := mode.build(context.Background())
				if err != ErrIntegrity || !reflect.DeepEqual(got, DocReadIndex{}) {
					test.Fatalf("wrong read length returned a partial or trusted projection: %v", err)
				}
			}
			spool.readTransform = nil
			spool.readError = fmt.Errorf("closed projection spool")
			got, err = mode.build(context.Background())
			if err != spool.readError || !reflect.DeepEqual(got, DocReadIndex{}) {
				test.Fatalf("read failure returned a partial projection or changed error: %v", err)
			}
			spool.readError = nil
			docReadProjectionCheck(test, mode, oracle)
		})
	}
}
