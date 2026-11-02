//go:build !race

package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
)

func legacyCanonicalReplacementCostSource(test testing.TB, count int) *legacyCanonicalNativeCostFixture {
	test.Helper()
	if count != 8 && count != 65 && count != 4097 {
		test.Fatal("replacement cost requires its finite inline/page/radix boundary fixtures", count)
	}
	event := CIREvent{Kind: EventCompaction, Seq: 17, ReplacementComplete: true, Replacement: make([]CIREvent, count)}
	for position := range event.Replacement {
		event.Replacement[position] = CIREvent{
			Kind: EventMessage, Seq: (count-1-position)/2 - count/4,
			ID: "replacement-cost-" + strconv.Itoa(position), Role: RoleAssistant,
		}
		if position%64 == 0 {
			event.Replacement[position].Blocks = []ContentBlock{{Type: "text", Text: strconv.Itoa(position) + " <&>\n\ud55c🙂\u2028\u2029\"\\"}}
		}
	}
	raw, err := json.Marshal(event)
	if err != nil {
		test.Fatal("replacement cost source generation failed", err)
	}
	normalized := canonicalEvents([]CIREvent{event})[0]
	value, err := canonicalEventJSONObject(normalized)
	if err != nil {
		test.Fatal("replacement cost original stable canonical oracle failed", err)
	}
	want, err := json.Marshal(value)
	if err != nil || len(raw) != len(want) || bytes.Equal(raw, want) || len(raw) > ConversationManifestChunkBytes ||
		len(raw) > 2<<20 || len(raw) > MaxFinalizedDocBytes {
		test.Fatal("replacement cost fixture lost its distinct bounded source/oracle spans", len(raw), len(want), err)
	}
	buffer := &legacySourceProofBuffer{body: raw}
	return &legacyCanonicalNativeCostFixture{buffer: buffer, source: buffer.input(), want: want, wantDigest: sha256.Sum256(want)}
}

type legacyCanonicalReplacementCostDrain struct {
	preparation    int
	output         int
	timedNext      int
	maxActions     int
	maxAccounted   int
	maxStorage     int
	inspectWork    uint64
	keyWork        uint64
	canonicalBytes int
}

func legacyCanonicalReplacementCostMeasure(benchmark *testing.B, fixture *legacyCanonicalNativeCostFixture, encoding *LegacyCanonicalEncoding, phase string) legacyCanonicalReplacementCostDrain {
	benchmark.Helper()
	ctx := context.Background()
	owner := encoding.nativeOwner
	digest := sha256.New()
	var cost legacyCanonicalReplacementCostDrain
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		preparation := encoding.preparation
		beforeInspect, beforeKeys := encoding.inspectWork, encoding.keyWork
		numberIndex, numberPosition := encoding.numbers.next, encoding.numbers.position
		timed := phase == "preparationNext" && preparation != nil || phase == "outputNext" && preparation == nil
		if timed {
			benchmark.StartTimer()
		}
		chunk, complete, err := encoding.Next(ctx)
		if timed {
			benchmark.StopTimer()
			cost.timedNext++
		}
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes ||
			encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > LegacyCanonicalEncodingBytes {
			benchmark.Fatal("replacement cost Next failed or exceeded the original output/storage quantum", stepIndex, err, len(chunk), cap(chunk), encoding.lastStorageBytes)
		}
		if encoding.inspectWork < beforeInspect || encoding.keyWork < beforeKeys {
			benchmark.Fatal("replacement cost inspection/ordering work decreased", stepIndex)
		}
		cost.maxStorage = max(cost.maxStorage, encoding.lastStorageBytes)
		if preparation != nil {
			cost.preparation++
			accounted := encoding.inspectWork - beforeInspect
			if len(chunk) != 0 || complete || accounted == 0 || accounted > legacyCanonicalEncodingActions ||
				preparation.lastActions <= 0 || preparation.lastActions > legacyCanonicalEncodingActions ||
				preparation.lastBytes < 0 || preparation.lastBytes > LegacyCanonicalEncodingBytes ||
				uint64(preparation.lastActions) != accounted || preparation.lastStorageBytes < 0 ||
				preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes ||
				(preparation.frames.length() == 0) != preparation.complete {
				benchmark.Fatal("replacement preparation did not remain empty, pending, and bounded", stepIndex, len(chunk), complete, accounted, preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes)
			}
			cost.maxActions = max(cost.maxActions, preparation.lastActions)
			cost.maxAccounted = max(cost.maxAccounted, preparation.lastBytes)
		} else {
			cost.output++
			if encoding.inspectWork != beforeInspect {
				benchmark.Fatal("replacement output restarted completed preparation", stepIndex)
			}
		}
		if len(chunk) > len(fixture.want)-cost.canonicalBytes || !bytes.Equal(chunk, fixture.want[cost.canonicalBytes:cost.canonicalBytes+len(chunk)]) {
			benchmark.Fatal("replacement cost changed the original stable canonical bytes", stepIndex, cost.canonicalBytes)
		}
		if _, err := digest.Write(chunk); err != nil {
			benchmark.Fatal(err)
		}
		cost.canonicalBytes += len(chunk)
		if !complete {
			if encoding.nativeOwner != owner {
				benchmark.Fatal("replacement cost released native ownership before completion", stepIndex)
			}
			if len(chunk) == 0 && encoding.inspectWork == beforeInspect && encoding.keyWork == beforeKeys &&
				encoding.numbers.next == numberIndex && encoding.numbers.position == numberPosition {
				benchmark.Fatal("replacement cost made no preparation, ordering, numeric, or output progress", stepIndex)
			}
			continue
		}
		var actualDigest [sha256.Size]byte
		copy(actualDigest[:], digest.Sum(nil))
		if cost.canonicalBytes != len(fixture.want) || actualDigest != fixture.wantDigest ||
			encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil ||
			encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 || encoding.rootHeap.bytes != 0 || encoding.pendingKeysHeap.bytes != 0 {
			benchmark.Fatal("replacement cost changed canonical extent/SHA-256 or retained terminal state", cost.canonicalBytes, len(fixture.want))
		}
		if owner != nil && (owner.event.Replacement != nil || owner.heap.bytes != 0 || owner.heap.err != nil || owner.seal != nil) {
			benchmark.Fatal("replacement cost completed without releasing the original native replacement")
		}
		cost.inspectWork, cost.keyWork = encoding.inspectWork, encoding.keyWork
		chunk, complete, err = encoding.Next(ctx)
		if err != nil || !complete || len(chunk) != 0 || encoding.inspectWork != cost.inspectWork || encoding.keyWork != cost.keyWork {
			benchmark.Fatal("replacement cost terminal retry changed completion or work", err, complete, len(chunk))
		}
		return cost
	}
	benchmark.Fatal("replacement cost exhausted its finite canonical Next allowance")
	return cost
}

func BenchmarkLegacyCanonicalReplacementCost(benchmark *testing.B) {
	benchmark.StopTimer()
	benchmark.Log("Parent-only serial non-race diagnostic: use -benchtime=1x -count=1 -cpu=1. Each leaf accepts only N=1; calibration beyond that is skipped. One sample is not a speedup, percentile, latency ceiling, or statistical performance claim.")
	benchmark.Log("Separate timed phases: old constructor = canonicalEvents + legacyCanonicalEventJSONObject + unowned Pending (including synchronous inspection); native constructor = newLegacyCanonicalEncodingNative only; native preparationNext = empty pending Next calls; outputNext = post-preparation Next calls, including remaining ordering/numeric work. The old path has no deferred preparation phase. Constructor-only savings are not whole canonicalization savings.")
	benchmark.Log("Fixture/oracle creation, authenticated source-part setup, fresh actual native decode (including the existing replacement fallback), both EOF probes, native owner transfer, byte/SHA/source-nonmutation checks, terminal retry, counters, and timer bookkeeping are outside the measured phase. Untimed phases still run to completion for every leaf. This is an internal post-decode comparison, not evidence of faster public native decoding, lower total RSS, end-to-end normalization, authorization/lease/CAS behavior, or durable restore.")
	benchmark.Log("B/op and allocs/op cover only the selected timer intervals, not untimed decode/source storage or whole-process RSS. Reservation counters are not physical allocation sizes or wall-clock bounds; per-Next timer transitions perturb these single-shot diagnostics. Source data is an unsorted event span, not a verified canonical document or publication proof.")
	benchmark.Log("The 8/65/4097 sizes count replacement children; sparse text blocks keep each raw event within one 512 KiB authenticated source part so the unchanged native decode helper retains its 512 KiB/two-load checks. Larger replacement fallback decoding is deliberately not covered or claimed bounded here.")
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || ConversationManifestChunkBytes != 512<<10 {
		benchmark.Fatal("replacement cost requires the original output/action/source quanta")
	}
	ctx := context.Background()
	for _, count := range []int{8, 65, 4097} {
		fixture := legacyCanonicalReplacementCostSource(benchmark, count)
		sourceDigest := sha256.Sum256(fixture.buffer.body)
		for _, path := range []struct {
			name   string
			native bool
		}{
			{name: "oldCanonicalEvents+legacyProjection+pending"},
			{name: "nativeOwned", native: true},
		} {
			phases := []string{"constructor", "outputNext"}
			if path.native {
				phases = []string{"constructor", "preparationNext", "outputNext"}
			}
			for _, phase := range phases {
				benchmark.Run(fmt.Sprintf("replacement=%d/%s/%s", count, path.name, phase), func(benchmark *testing.B) {
					benchmark.StopTimer()
					if benchmark.N != 1 {
						benchmark.Skip("finite replacement cost probe requires -benchtime=1x; repeated calibration is disabled")
					}
					benchmark.ReportAllocs()
					benchmark.ResetTimer()
					native, event, decodeSteps := legacyCanonicalNativeCostDecode(benchmark, fixture)
					before, err := json.Marshal(event)
					if err != nil || !bytes.Equal(before, fixture.buffer.body) || len(event.Replacement) != count || !native.canonicalReady {
						benchmark.Fatal("replacement cost actual native decode changed the unsorted source event", count, err)
					}
					if err := legacySourceEOF(ctx, fixture.buffer.Read, fixture.source.Bytes); err != nil {
						benchmark.Fatal("replacement cost source EOF probes failed", err)
					}
					var eofZero, eofOne int
					for _, read := range fixture.buffer.reads {
						if read[0] == fixture.source.Bytes && read[1] == 0 {
							eofZero++
							continue
						}
						if read[0] == fixture.source.Bytes && read[1] == 1 {
							eofOne++
							continue
						}
						if read[0] < 0 || read[0] >= fixture.source.Bytes || read[0]%ConversationManifestChunkBytes != 0 ||
							read[1] != min(ConversationManifestChunkBytes, fixture.source.Bytes-read[0]) {
							benchmark.Fatal("replacement cost decode bypassed full authenticated source-part reads", read)
						}
					}
					if eofZero != 1 || eofOne != 1 {
						benchmark.Fatal("replacement cost omitted either source EOF probe", eofZero, eofOne)
					}
					reads := len(fixture.buffer.reads)
					var owner *legacyNativeCanonicalOwner
					if path.native {
						owner, err = native.takeCanonicalOwner(ctx)
						if err != nil || owner == nil {
							benchmark.Fatal("replacement cost did not transfer an actual fresh native owner", err)
						}
					} else {
						native.releaseValues()
					}
					var encoding *LegacyCanonicalEncoding
					if phase == "constructor" {
						benchmark.StartTimer()
					}
					if path.native {
						encoding, err = newLegacyCanonicalEncodingNative(ctx, owner)
					} else {
						normalized := canonicalEvents([]CIREvent{event})[0]
						var value map[string]any
						value, err = legacyCanonicalEventJSONObject(normalized)
						if err == nil {
							encoding, err = newLegacyCanonicalEncodingPending(ctx, value)
						}
					}
					if phase == "constructor" {
						benchmark.StopTimer()
					}
					if err != nil || encoding == nil {
						benchmark.Fatal("replacement cost constructor failed", err)
					}
					defer encoding.release()
					initialInspect, initialMapKeys := encoding.inspectWork, encoding.mapKeys.length()
					if path.native {
						if encoding.nativeOwner != owner || encoding.preparation == nil || encoding.complete || initialInspect != 0 ||
							encoding.keyWork != 0 || initialMapKeys != 0 || encoding.frames.length() != 0 || encoding.preparation.frames.length() != 1 {
							benchmark.Fatal("replacement native constructor visited children/keys or lost its pending owner")
						}
						root := encoding.preparation.frames.at(0)
						if root == nil || root.kind != 0 || root.phase != 0 || root.depth != 2 || !root.prepare || encoding.preparation.complete {
							benchmark.Fatal("replacement native constructor did not leave one unvisited preparation root")
						}
					} else if encoding.nativeOwner != nil || encoding.preparation != nil {
						benchmark.Fatal("replacement old pending constructor stopped being synchronous and unowned")
					}
					cost := legacyCanonicalReplacementCostMeasure(benchmark, fixture, encoding, phase)
					if path.native && cost.preparation < 2 || !path.native && cost.preparation != 0 || cost.output == 0 ||
						phase != "constructor" && cost.timedNext == 0 {
						benchmark.Fatal("replacement cost did not exercise its expected separate phases", cost.preparation, cost.output, cost.timedNext)
					}
					after, err := json.Marshal(event)
					if err != nil || !bytes.Equal(before, after) || sha256.Sum256(fixture.buffer.body) != sourceDigest || len(fixture.buffer.reads) != reads {
						benchmark.Fatal("replacement canonicalization mutated the source event/bytes or reread source", err)
					}
					benchmark.ReportMetric(float64(initialInspect), "constructor-inspect-actions")
					benchmark.ReportMetric(float64(initialMapKeys), "constructor-mapkeys")
					benchmark.ReportMetric(float64(cost.preparation), "prep-Next/op")
					benchmark.ReportMetric(float64(cost.output), "output-Next/op")
					benchmark.ReportMetric(float64(cost.timedNext), "timed-Next/op")
					benchmark.ReportMetric(float64(cost.maxActions), "max-prep-actions/Next")
					benchmark.ReportMetric(float64(cost.maxAccounted), "max-prep-accounted-B/Next")
					benchmark.ReportMetric(float64(cost.maxStorage), "max-storage-work-B/Next")
					benchmark.ReportMetric(float64(cost.inspectWork), "inspect-actions/op")
					benchmark.ReportMetric(float64(cost.keyWork), "key-work/op")
					benchmark.ReportMetric(float64(decodeSteps), "untimed-native-decode-steps/op")
					benchmark.ReportMetric(float64(fixture.source.Bytes), "untimed-source-B/fixture")
					benchmark.ReportMetric(float64(cost.canonicalBytes), "canonical-B/op")
					benchmark.Logf("One event; timed phase=%s; untimed actual native decode=%d steps over %d authenticated source parts with EOF=%d/%d; stable old canonical oracle matched exactly, SHA-256=%x. No public decode or total RSS improvement is inferred.", phase, decodeSteps, len(fixture.source.Parts), eofZero, eofOne, fixture.wantDigest)
				})
			}
		}
	}
}
