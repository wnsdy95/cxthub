package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"testing"
	"unsafe"
)

type legacyGeometryCost struct {
	proofSteps         int
	steps              int
	nextField          int
	maxWork            int
	maxLoads           int
	maxFields          int
	maxValues          int
	work               int64
	semantic           int64
	reads              legacyNativeEventsL06Reads
	peakNativeRetained int64
	peakInputRetained  int64
	peakRetained       int64
	manifestRetained   int64
}

func legacyGeometryCostReads(test testing.TB, buffer *legacySourceProofBuffer, partBytes, from int) legacyNativeEventsL06Reads {
	test.Helper()
	var cost legacyNativeEventsL06Reads
	for _, read := range buffer.reads[from:] {
		switch {
		case read == [2]int{len(buffer.body), 0}:
			cost.zero++
		case read == [2]int{len(buffer.body), 1}:
			cost.one++
		case read[0] >= 0 && read[0] < len(buffer.body) && read[0]%partBytes == 0 &&
			read[1] == min(partBytes, len(buffer.body)-read[0]):
			cost.loads++
			cost.bytes += int64(read[1])
		default:
			test.Fatal("geometry cost bypassed complete authenticated source-part access", partBytes, read)
		}
	}
	return cost
}

func legacyGeometryCostMeasure(test testing.TB, fixture *legacyNativeEventsL06Fixture, geometry int, benchmark *testing.B) legacyGeometryCost {
	test.Helper()
	if ConversationManifestChunkBytes != 512<<10 || LegacyNormalizationPartBytes != 64<<10 ||
		LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || LegacyNormalizationStepEvents != 64 ||
		geometry != 0 && geometry != LegacyNormalizationPartBytes {
		test.Fatal("geometry cost requires the unchanged source, canonical, and action quanta", geometry)
	}
	ctx := context.Background()
	buffer, source, proofSteps := legacyNativeEventsL06Producer(test, fixture)
	if source.PartBytes != 0 || len(buffer.reads) != 0 {
		test.Fatal("geometry cost changed the original default-512 KiB producer or its checked read trace")
	}
	partBytes := ConversationManifestChunkBytes
	if geometry != 0 {
		partBytes = geometry
		source.PartBytes = geometry
		source.Parts = make([]ConversationManifestChunk, 0, (source.Bytes+partBytes-1)/partBytes)
		for offset := 0; offset < source.Bytes; offset += partBytes {
			body := buffer.body[offset:min(offset+partBytes, source.Bytes)]
			source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))})
		}
	}
	var manifest docRangeHeaderHeapEstimate
	manifest.allocation(int64(cap(source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	for _, part := range source.Parts {
		manifest.allocation(int64(len(part.Hash)), 1)
	}
	if manifest.err != nil || manifest.bytes <= 0 {
		test.Fatal("geometry cost lost its actual source manifest ledger", manifest)
	}
	if benchmark != nil {
		benchmark.StartTimer()
	}
	native, err := newLegacyNormalizationNativeEvent()
	if err == nil {
		native.reset(fixture.eventStart, source.Bytes)
		native.object.prefix = true
	}
	if benchmark != nil {
		benchmark.StopTimer()
	}
	if err != nil || native == nil || native.object.metadata != &legacyEventNativeFields || len(buffer.reads) != 0 {
		test.Fatal("geometry cost actual native constructor failed or accessed the source", err)
	}
	defer native.releaseValues()
	var frame legacyJSONValueScanner
	cost := legacyGeometryCost{proofSteps: proofSteps, manifestRetained: manifest.bytes}
	for quantum := 0; quantum < 2048; quantum++ {
		before, position := len(buffer.reads), native.object.progressPosition()
		if benchmark != nil {
			benchmark.StartTimer()
		}
		input := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
		event, complete, failure := native.step(ctx, input, &frame)
		if benchmark != nil {
			benchmark.StopTimer()
		}
		cost.steps++
		reads := legacyGeometryCostReads(test, buffer, partBytes, before)
		if failure != nil || input.failure != nil || input.unbounded || reads.loads != input.loads || reads.zero != 0 || reads.one != 0 ||
			input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || reads.bytes > int64(2*partBytes) ||
			input.fields < 0 || input.fields > native.object.fieldActionLimit() || input.values < 0 || input.values > legacyNativeAnyValueActions ||
			input.eventsRunning != nil || input.eventsPending != nil || input.eventsFieldLimit != 0 {
			test.Fatal("geometry cost actual native Step escaped the unchanged work/two-load/field/value guards", quantum, failure, input.failure, input.workBytes, input.loads, input.fields, input.values)
		}
		cost.work += int64(input.workBytes)
		cost.semantic += int64(input.semanticBytes)
		cost.maxWork, cost.maxLoads = max(cost.maxWork, input.workBytes), max(cost.maxLoads, input.loads)
		cost.maxFields, cost.maxValues = max(cost.maxFields, input.fields), max(cost.maxValues, input.values)
		if next := native.object.progressPosition(); next < position || next > fixture.eventStart+len(fixture.event) || next-position > input.workBytes {
			test.Fatal("geometry cost actual native cursor lost monotonic accounted progress", position, next, input.workBytes)
		}
		retained, retainedErr := native.retained()
		var inputHeap docRangeHeaderHeapEstimate
		inputHeap.allocation(1, int64(unsafe.Sizeof(*input)))
		for _, page := range input.pages {
			if len(page) > partBytes || cap(page) > partBytes {
				test.Fatal("geometry cost retained an oversized authenticated input page", len(page), cap(page), partBytes)
			}
			inputHeap.allocation(int64(cap(page)), 1)
		}
		live := manifest
		live.include(docRangeHeaderHeapEstimate{bytes: retained, err: retainedErr})
		live.include(inputHeap)
		live.allocation(1, int64(unsafe.Sizeof(frame)))
		if retainedErr != nil || retained <= 0 || inputHeap.err != nil || live.err != nil || live.bytes < retained {
			test.Fatal("geometry cost omitted native, current input pages, frame, or manifest retention", retained, retainedErr, inputHeap, live)
		}
		cost.peakNativeRetained = max(cost.peakNativeRetained, retained)
		cost.peakInputRetained = max(cost.peakInputRetained, inputHeap.bytes)
		cost.peakRetained = max(cost.peakRetained, live.bytes)
		if native.decoded.ReplacementComplete && cost.nextField == 0 {
			cost.nextField = cost.steps
		}
		for _, page := range input.pages {
			clear(page)
		}
		if !complete {
			if native.canonicalReady {
				test.Fatal("geometry cost partial event became canonical ready before its following field")
			}
			continue
		}
		typed, marshalErr := json.Marshal(event)
		minimum, heapErr := legacyNativeValueHeap(ctx, reflect.ValueOf(event))
		if marshalErr != nil || heapErr != nil || minimum.err != nil || retained < minimum.bytes ||
			!native.canonicalReady || cost.nextField == 0 || !event.ReplacementComplete ||
			native.object.position != fixture.eventStart+len(fixture.event) || !reflect.DeepEqual(event, fixture.expected) ||
			!bytes.Equal(typed, fixture.typedJSON) || sha256.Sum256(typed) != fixture.typedSHA {
			test.Fatal("geometry cost changed exact typed values, following field, bytes/SHA-256, or visible heap coverage", marshalErr, heapErr, retained, minimum)
		}
		if err := legacySourceEOF(ctx, source.Read, source.Bytes); err != nil {
			test.Fatal("geometry cost actual native source failed its final EOF probes", err)
		}
		cost.reads = legacyGeometryCostReads(test, buffer, partBytes, 0)
		visited := make([]bool, len(source.Parts))
		for _, read := range buffer.reads {
			if read[0] < source.Bytes {
				visited[read[0]/partBytes] = true
			}
		}
		for ordinal, seen := range visited {
			if !seen {
				test.Fatal("geometry cost native decode omitted an authenticated source part", ordinal, partBytes)
			}
		}
		if cost.reads.bytes < int64(source.Bytes) || cost.reads.zero != 1 || cost.reads.one != 1 ||
			!bytes.Equal(buffer.body, fixture.raw) || !bytes.Equal(buffer.body[fixture.eventStart:fixture.eventStart+len(fixture.event)], fixture.event) ||
			sha256.Sum256(buffer.body) != fixture.bytesSHA || sha256.Sum256(fixture.raw) != fixture.bytesSHA ||
			!reflect.DeepEqual(legacyEventNativeFields, fixture.metadata) || native.object.metadata != &legacyEventNativeFields {
			test.Fatal("geometry cost changed original source bytes/SHA, final EOF, or global native metadata", cost.reads)
		}
		if geometry == LegacyNormalizationPartBytes && len(fixture.expected.Replacement) == 8192 &&
			cost.reads.bytes+int64(len(fixture.raw)) >= 8*int64(len(fixture.raw)) {
			test.Fatal("64 KiB actual native source exceeded the strict total-read <8*source guard, including original admission", cost.reads.bytes, len(fixture.raw))
		}
		return cost
	}
	test.Fatal("geometry cost actual native driver exhausted its finite Step allowance")
	return cost
}

func TestLegacyGeometryCostActualNativeSourceParity(test *testing.T) {
	for _, shape := range legacyNativeEventsL06Cases() {
		test.Run(shape.name, func(test *testing.T) {
			fixture := legacyNativeEventsL06Source(test, shape)
			baseline := legacyGeometryCostMeasure(test, fixture, 0, nil)
			actual := legacyGeometryCostMeasure(test, fixture, LegacyNormalizationPartBytes, nil)
			if baseline.proofSteps != actual.proofSteps || len(fixture.event) > ConversationManifestChunkBytes && (baseline.steps <= 1 || actual.steps <= 1) {
				test.Fatal("geometry A/B changed original admission or collapsed large native decoding into one Step", baseline, actual)
			}
			for _, observation := range []struct {
				name string
				cost legacyGeometryCost
			}{
				{name: "default512KiB-diagnostic", cost: baseline},
				{name: "native64KiB", cost: actual},
			} {
				cost := observation.cost
				total := cost.reads.bytes + int64(len(fixture.raw))
				test.Logf("%s source=%d event=%d proof-steps=%d proof-read-B=%d proof-EOF=2 native-quanta=%d part-reads=%d part-read-B=%d final-EOF=%d total-read-B=%d total-read-amplification=%.4fx under-8x=%t work-B=%d semantic-B=%d max-work=%d max-loads=%d max-fields=%d max-values=%d next-field=%d native-retained-peak=%d input-retained-peak=%d manifest-retained=%d observed-retained-peak=%d raw-SHA=%x typed-SHA=%x", observation.name, len(fixture.raw), len(fixture.event), cost.proofSteps, len(fixture.raw), cost.steps, cost.reads.loads, cost.reads.bytes, cost.reads.zero+cost.reads.one, total, float64(total)/float64(len(fixture.raw)), total < 8*int64(len(fixture.raw)), cost.work, cost.semantic, cost.maxWork, cost.maxLoads, cost.maxFields, cost.maxValues, cost.nextField, cost.peakNativeRetained, cost.peakInputRetained, cost.manifestRetained, cost.peakRetained, fixture.bytesSHA, fixture.typedSHA)
			}
			test.Log("Both geometries use actual native metadata, never the old fallback. The historical default-512 KiB 25.64x total-read result fails the <8x criterion and is diagnostic, not a passing cost result. The 64 KiB 8192-child guard includes the original 512 KiB admission read. Retained values are conservative ledgers observed at Step boundaries, not allocator/RSS peaks; fixture/oracle/body/read-history storage and completed producer retention are excluded. This is not canonical-output, publication, durable-resume, total-speedup, p95, or operating-capacity evidence.")
		})
	}
}

func BenchmarkLegacyGeometryCostActualNativeSource(benchmark *testing.B) {
	benchmark.StopTimer()
	for name, expected := range map[string]string{"test.benchtime": "1x", "test.count": "1", "test.cpu": "1"} {
		configured := flag.Lookup(name)
		if configured == nil || configured.Value.String() != expected {
			benchmark.Skip("parent-only serial single-shot diagnostic requires -benchtime=1x -count=1 -cpu=1")
		}
	}
	benchmark.Log("Exactly the existing small8/replacement8192/largeChild1MiB cases, default-512 KiB versus 64 KiB on the same actual native driver; no global metadata or old fallback switch. Each leaf runs once.")
	benchmark.Log("Timed intervals: actual native constructor/reset/prefix plus each fresh shared input/native.step call, including authenticated source reads, copies, and hashes. Untimed: fixture/typed oracle, original 512 KiB source producer and its hash/EOF admission, 64 KiB manifest setup, retention/read/cursor/budget checks, page poisoning, typed equality/JSON/SHA, final EOF, release, and timer bookkeeping. Untimed admission bytes are included in total-source-read metrics. Per-Step timer transitions perturb this single-shot diagnostic; allocation metrics cover timed intervals only, not the full lifecycle.")
	benchmark.Log("Retained metrics are Step-boundary native/input/manifest/frame ledgers, excluding fixture/oracle/body/history and completed producer storage; they are not physical heap/RSS peaks. The historical default-512 KiB 25.64x is a failing-cost diagnostic. Only the 64 KiB 8192-child actual source is gated by strict total-read <8*source. Original 512 KiB/two-load/field/value and producer <2 MiB guards remain; canonical 64 KiB/1024, 16 MiB/64 MiB, depth, 128k/setup30s, current authorization/incarnation/version/lease/CAS are not changed or certified. No total-speedup, p95, RSS, DB/network/publication, or durable-restore claim.")
	for _, shape := range legacyNativeEventsL06Cases() {
		fixture := legacyNativeEventsL06Source(benchmark, shape)
		for _, geometry := range []int{0, LegacyNormalizationPartBytes} {
			name := "default512KiB-diagnostic"
			if geometry != 0 {
				name = "native64KiB"
			}
			benchmark.Run(fmt.Sprintf("%s/%s/nativeConstructorAndSteps", shape.name, name), func(benchmark *testing.B) {
				benchmark.StopTimer()
				if benchmark.N != 1 {
					benchmark.Skip("geometry cost disables repeated calibration; use -benchtime=1x")
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				cost := legacyGeometryCostMeasure(benchmark, fixture, geometry, benchmark)
				total := cost.reads.bytes + int64(len(fixture.raw))
				benchmark.ReportMetric(float64(len(fixture.raw)), "source-B")
				benchmark.ReportMetric(float64(cost.proofSteps), "untimed-proof-quanta/op")
				benchmark.ReportMetric(float64(len(fixture.raw)), "untimed-proof-read-B/op")
				benchmark.ReportMetric(2, "untimed-proof-EOF/op")
				benchmark.ReportMetric(float64(cost.steps), "native-quanta/op")
				benchmark.ReportMetric(float64(cost.reads.loads), "native-part-reads/op")
				benchmark.ReportMetric(float64(cost.reads.bytes), "native-part-read-B/op")
				benchmark.ReportMetric(float64(cost.reads.zero+cost.reads.one), "untimed-final-EOF/op")
				benchmark.ReportMetric(float64(total), "total-source-read-B/op")
				benchmark.ReportMetric(float64(total)/float64(len(fixture.raw)), "total-source-read-amplification")
				benchmark.ReportMetric(float64(cost.work), "native-work-B/op")
				benchmark.ReportMetric(float64(cost.semantic), "native-semantic-B/op")
				benchmark.ReportMetric(float64(cost.maxWork), "max-work-B/step")
				benchmark.ReportMetric(float64(cost.maxLoads), "max-loads/step")
				benchmark.ReportMetric(float64(cost.maxFields), "max-fields/step")
				benchmark.ReportMetric(float64(cost.maxValues), "max-values/step")
				benchmark.ReportMetric(float64(cost.nextField), "following-field-quantum")
				benchmark.ReportMetric(float64(cost.peakNativeRetained), "observed-native-retained-B")
				benchmark.ReportMetric(float64(cost.peakInputRetained), "observed-input-retained-B")
				benchmark.ReportMetric(float64(cost.manifestRetained), "manifest-retained-B")
				benchmark.ReportMetric(float64(cost.peakRetained), "observed-live-retained-B")
				benchmark.Logf("raw-SHA=%x typed-SHA=%x; total-read-under-8x=%t; default512KiB is diagnostic, not an acceptance gate", fixture.bytesSHA, fixture.typedSHA, total < 8*int64(len(fixture.raw)))
			})
		}
	}
}
