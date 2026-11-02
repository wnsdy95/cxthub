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

type legacyCanonicalNativeCostFixture struct {
	buffer         *legacySourceProofBuffer
	source         LegacyByteSource
	want           []byte
	wantDigest     [sha256.Size]byte
	eventStart     int
	proofSteps     int
	proofLoads     int
	proofReadBytes int64
	proofEOF       int
}

func legacyCanonicalNativeCostEvent(shape string, count int) CIREvent {
	created := json.Number("1787683260.123456789")
	event := CIREvent{
		Seq:              17,
		ProviderMetadata: &ProviderMetadata{TurnID: "native-constructor-cost", CreateTime: &created},
	}
	switch shape {
	case "blocks":
		event.Kind, event.Role = EventMessage, RoleAssistant
		event.Blocks = make([]ContentBlock, count)
		for position := range event.Blocks {
			event.Blocks[position] = ContentBlock{Type: "text", Text: strconv.Itoa(position) + " <&>\n\ud55c🙂\u2028\u2029\"\\"}
		}
	case "map":
		event.Kind, event.CallID = EventToolResult, "native-constructor-cost"
		output := make(map[string]any, count)
		for position := 0; position < count; position++ {
			output[fmt.Sprintf("key-%06d", count-position)] = strconv.Itoa(position) + " <&>\n\ud55c🙂\u2028\u2029\"\\"
		}
		event.Output = output
	}
	return event
}

func legacyCanonicalNativeCostSource(test testing.TB, shape string, count int) *legacyCanonicalNativeCostFixture {
	test.Helper()
	ctx := context.Background()
	event := legacyCanonicalNativeCostEvent(shape, count)
	value, err := canonicalEventJSONObject(event)
	if err != nil {
		test.Fatal("cost fixture projection failed", err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal("cost fixture canonical oracle failed", err)
	}
	header, err := canonicalJSON(CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, Fidelity: FidelityFull})
	if err != nil {
		test.Fatal("cost fixture envelope failed", err)
	}
	raw := AssembleCanonicalDoc(header, [][]byte{want})
	buffer := &legacySourceProofBuffer{body: raw}
	fixture := &legacyCanonicalNativeCostFixture{
		buffer: buffer, source: buffer.input(), want: want, wantDigest: sha256.Sum256(want),
		eventStart: len(canonicalDocPrefix) + len(header) + len(canonicalDocMiddle),
	}
	target := &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	proof, err := NewLegacyByteSourcePreparation(ctx, HashContent(raw), fixture.source, target, &verifier)
	if err != nil {
		test.Fatal("cost fixture initial source proof failed", err)
	}
	for allowance := 64 + 32*len(fixture.source.Parts) + len(raw)/LegacyNormalizationStepEvents; fixture.proofSteps < allowance; {
		progress, err := proof.Step(ctx)
		fixture.proofSteps++
		if err != nil {
			test.Fatal("cost fixture initial source proof rejected canonical input", err)
		}
		if progress == LegacyByteSourcePending {
			continue
		}
		verified, err := proof.Result()
		if progress != LegacyByteSourceVerified || err != nil || !verified.Valid() || !bytes.Equal(target.body, want) {
			test.Fatal("cost fixture did not obtain the original canonical source proof", progress, err)
		}
		for _, read := range buffer.reads {
			if read[0] == len(raw) && (read[1] == 0 || read[1] == 1) {
				fixture.proofEOF++
				continue
			}
			if read[0] < 0 || read[0] >= len(raw) || read[0]%ConversationManifestChunkBytes != 0 ||
				read[1] != min(ConversationManifestChunkBytes, len(raw)-read[0]) {
				test.Fatal("cost source proof bypassed authenticated full-part reads", read)
			}
			fixture.proofLoads++
			fixture.proofReadBytes += int64(read[1])
		}
		if fixture.proofEOF != 2 || fixture.proofReadBytes < int64(len(raw)) {
			test.Fatal("cost source proof omitted source bytes or either EOF probe", fixture.proofReadBytes, fixture.proofEOF)
		}
		buffer.reads = nil
		return fixture
	}
	test.Fatal("cost fixture source proof exhausted its finite allowance")
	return nil
}

func legacyCanonicalNativeCostDecode(test testing.TB, fixture *legacyCanonicalNativeCostFixture) (*legacyNormalizationNativeEvent, CIREvent, int) {
	test.Helper()
	ctx := context.Background()
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("cost fixture native decoder failed", err)
	}
	native.reset(fixture.eventStart, fixture.eventStart+len(fixture.want))
	fixture.buffer.reads = nil
	var frame legacyJSONValueScanner
	allowance := 64 + 32*len(fixture.source.Parts) + len(fixture.want)/LegacyNormalizationStepEvents
	for stepIndex := 0; stepIndex < allowance; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
		event, complete, err := native.step(ctx, input, &frame)
		if err != nil || input.failure != nil {
			test.Fatal("cost fixture native decode failed", stepIndex, err, input.failure)
		}
		if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields > LegacyNormalizationStepEvents*len(native.object.metadata.fields) {
			test.Fatal("cost native decode escaped its existing quantum", input.workBytes, input.loads, input.fields)
		}
		if complete {
			return native, event, stepIndex + 1
		}
	}
	test.Fatal("cost fixture native decode exhausted its finite allowance")
	return nil, CIREvent{}, 0
}

type legacyCanonicalNativeCostDrain struct {
	retained       int64
	mapKeys        int
	initialInspect uint64
	preparation    int
	maxActions     int
	maxAccounted   uint64
	inspectWork    uint64
	nextCalls      int
	digest         [sha256.Size]byte
}

func legacyCanonicalNativeCostMeasure(test testing.TB, fixture *legacyCanonicalNativeCostFixture, encoding *LegacyCanonicalEncoding) legacyCanonicalNativeCostDrain {
	test.Helper()
	retained, err := encoding.RetainedBytes()
	if err != nil || retained <= 0 {
		test.Fatal("cost constructor lost initial retained accounting", retained, err)
	}
	cost := legacyCanonicalNativeCostDrain{retained: retained, mapKeys: encoding.mapKeys.length(), initialInspect: encoding.inspectWork}
	owner := encoding.nativeOwner
	digest := sha256.New()
	position := 0
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		preparation, before := encoding.preparation, encoding.inspectWork
		chunk, complete, err := encoding.Next(context.Background())
		cost.nextCalls++
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatal("cost canonical drain failed or escaped its chunk bound", err, len(chunk), cap(chunk))
		}
		if encoding.inspectWork < before {
			test.Fatal("cost canonical inspection accounting decreased", before, encoding.inspectWork)
		}
		accounted := encoding.inspectWork - before
		if preparation != nil {
			cost.preparation++
			cost.maxActions = max(cost.maxActions, preparation.lastActions)
			cost.maxAccounted = max(cost.maxAccounted, uint64(max(0, preparation.lastBytes)))
			if len(chunk) != 0 || complete || accounted > legacyCanonicalEncodingActions ||
				preparation.lastActions < 0 || preparation.lastActions > legacyCanonicalEncodingActions ||
				preparation.lastBytes < 0 || preparation.lastBytes > LegacyCanonicalEncodingBytes || uint64(preparation.lastActions) != accounted {
				test.Fatal("cost preparation exposed output or exceeded its accounted quantum", len(chunk), complete, accounted, preparation.lastActions, preparation.lastBytes)
			}
		}
		if len(chunk) > len(fixture.want)-position || !bytes.Equal(chunk, fixture.want[position:position+len(chunk)]) {
			test.Fatal("old/native constructor changed exact canonical bytes", stepIndex, position)
		}
		if _, err := digest.Write(chunk); err != nil {
			test.Fatal(err)
		}
		position += len(chunk)
		if !complete {
			if encoding.nativeOwner != owner {
				test.Fatal("cost drain released native ownership before completion")
			}
			continue
		}
		copy(cost.digest[:], digest.Sum(nil))
		cost.inspectWork = encoding.inspectWork
		if position != len(fixture.want) || cost.digest != fixture.wantDigest || encoding.nativeOwner != nil || encoding.preparation != nil {
			test.Fatal("cost drain changed the eventual canonical digest, extent, or terminal ownership", position, len(fixture.want))
		}
		return cost
	}
	test.Fatal("cost canonical drain exhausted its finite Next allowance")
	return cost
}

func BenchmarkLegacyCanonicalNativeConstructor(benchmark *testing.B) {
	benchmark.StopTimer()
	benchmark.Log("Constructor-only timing and allocations: old legacyCanonicalEventJSONObject+unowned Pending versus owned Native. Source generation, initial source proof, fresh native decoding on both paths, one-use owner transfer on every native iteration, initial RetainedBytes, preparation Next calls, and exact canonical byte/digest checks are untimed. This does not measure whole-lifecycle cost or bound atomic allocation latency; use a finite serial benchtime such as 1x.")
	benchmark.Log("Initial native retention conservatively charges the entire owner; the old projected heap may legitimately be smaller. max-prep-accounted-B/Next is traversal accounting, not an allocation-size or wall-clock bound. Durable restore remains outside this benchmark.")
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || ConversationManifestChunkBytes != 512<<10 {
		benchmark.Fatal("cost benchmark requires the existing encoder, action, and native source quanta")
	}
	ctx := context.Background()
	for _, shape := range []string{"blocks", "map"} {
		for _, count := range []int{1 << 10, 16 << 10, 64 << 10} {
			fixture := legacyCanonicalNativeCostSource(benchmark, shape, count)
			for _, path := range []struct {
				name   string
				native bool
			}{
				{name: "legacyProjection+unownedPending"},
				{name: "nativeOwned", native: true},
			} {
				benchmark.Run(fmt.Sprintf("%s=%d/%s", shape, count, path.name), func(benchmark *testing.B) {
					benchmark.StopTimer()
					benchmark.ReportAllocs()
					benchmark.ResetTimer()
					var retained, mapKeys, initialInspect, preparation, inspectWork, nextCalls, decodeSteps float64
					var maxActions int
					var maxAccounted uint64
					var digest [sha256.Size]byte
					for operation := 0; operation < benchmark.N; operation++ {
						native, event, steps := legacyCanonicalNativeCostDecode(benchmark, fixture)
						decodeSteps += float64(steps)
						var owner *legacyNativeCanonicalOwner
						if path.native {
							var err error
							owner, err = native.takeCanonicalOwner(ctx)
							if err != nil || owner == nil {
								benchmark.Fatal("cost fixture did not transfer an actual fresh native owner", err)
							}
						} else {
							native.releaseValues()
						}
						benchmark.StartTimer()
						var encoding *LegacyCanonicalEncoding
						var err error
						if path.native {
							encoding, err = newLegacyCanonicalEncodingNative(ctx, owner)
						} else {
							var value map[string]any
							value, err = legacyCanonicalEventJSONObject(event)
							if err == nil {
								encoding, err = newLegacyCanonicalEncodingPending(ctx, value)
							}
						}
						benchmark.StopTimer()
						if err != nil || encoding == nil {
							benchmark.Fatal("cost constructor failed", err)
						}
						if path.native {
							if encoding.preparation == nil || encoding.nativeOwner != owner || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 {
								benchmark.Fatal("native constructor eagerly visited children/keys or lost its actual owner")
							}
						} else if encoding.preparation != nil || encoding.nativeOwner != nil {
							benchmark.Fatal("old pending constructor stopped being synchronous and unowned")
						}
						cost := legacyCanonicalNativeCostMeasure(benchmark, fixture, encoding)
						if path.native && cost.preparation == 0 {
							benchmark.Fatal("native constructor deferred no inspection to Next")
						}
						retained += float64(cost.retained)
						mapKeys += float64(cost.mapKeys)
						initialInspect += float64(cost.initialInspect)
						preparation += float64(cost.preparation)
						inspectWork += float64(cost.inspectWork)
						nextCalls += float64(cost.nextCalls)
						maxActions = max(maxActions, cost.maxActions)
						maxAccounted = max(maxAccounted, cost.maxAccounted)
						digest = cost.digest
					}
					operations := float64(benchmark.N)
					benchmark.ReportMetric(retained/operations, "constructor-retained-B")
					benchmark.ReportMetric(mapKeys/operations, "constructor-mapkeys")
					benchmark.ReportMetric(initialInspect/operations, "constructor-inspect-actions")
					benchmark.ReportMetric(preparation/operations, "prep-Next/op")
					benchmark.ReportMetric(inspectWork/operations, "inspect-actions/op")
					benchmark.ReportMetric(float64(maxActions), "max-prep-actions/Next")
					benchmark.ReportMetric(float64(maxAccounted), "max-prep-accounted-B/Next")
					benchmark.ReportMetric(nextCalls/operations, "Next/op")
					benchmark.ReportMetric(decodeSteps/operations, "untimed-native-decode-steps/op")
					benchmark.ReportMetric(float64(len(fixture.buffer.body)), "untimed-source-B/fixture")
					benchmark.ReportMetric(float64(fixture.proofSteps), "untimed-initial-proof-steps/fixture")
					benchmark.ReportMetric(float64(len(fixture.want)), "canonical-B")
					benchmark.Logf("Untimed source generation: %d bytes, %d parts; initial source proof: %d steps, %d authenticated loads, %d read bytes, %d EOF probes. Both paths decode fresh native storage untimed per operation; native additionally transfers a fresh one-use owner untimed. Eventual exact old/new canonical SHA-256: %x. Whole lifecycle is not measured.", len(fixture.buffer.body), len(fixture.source.Parts), fixture.proofSteps, fixture.proofLoads, fixture.proofReadBytes, fixture.proofEOF, digest)
				})
			}
		}
	}
}
