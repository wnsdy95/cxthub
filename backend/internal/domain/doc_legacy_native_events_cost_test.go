package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeEventsL06Case struct {
	name     string
	children int
	text     int
}

func legacyNativeEventsL06Cases() []legacyNativeEventsL06Case {
	return []legacyNativeEventsL06Case{
		{name: "small8", children: 8, text: 64},
		{name: "replacement8192", children: 8192, text: 64},
		{name: "largeChild1MiB", children: 1, text: 2*ConversationManifestChunkBytes + 73},
	}
}

type legacyNativeEventsL06Fixture struct {
	raw        []byte
	event      []byte
	typedJSON  []byte
	expected   CIREvent
	reference  ContentHash
	bytesSHA   [sha256.Size]byte
	typedSHA   [sha256.Size]byte
	metadata   legacyNativeObjectFields
	eventStart int
}

func legacyNativeEventsL06Source(test testing.TB, shape legacyNativeEventsL06Case) *legacyNativeEventsL06Fixture {
	test.Helper()
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", shape.text) + `"}]}`
	event := []byte(`{"kind":"compaction","replacement":[` + strings.Repeat(child+",", shape.children-1) + child + `],"replacement_complete":true}`)
	if shape.children == 8192 && len(event) != 1138753 {
		test.Fatal("L06 lost the original 8192-child source reproduction", len(event))
	}
	const prefix = `{"envelope":{"cir_version":"2"},"events":[`
	raw := append(append([]byte(prefix), event...), ']', '}')
	canonical, proof, err := legacyNormalizationFrameOracle(raw)
	if err != nil || !proof.Valid() || bytes.Equal(raw, canonical) || proof.Hash() != HashContent(canonical) {
		test.Fatal("L06 source lost its independent original typed canonical oracle", err)
	}
	fixture := &legacyNativeEventsL06Fixture{
		raw: raw, event: event, reference: proof.Hash(), bytesSHA: sha256.Sum256(raw), eventStart: len(prefix),
		metadata: legacyEventNativeFields,
	}
	fixture.metadata.fields = append([]legacyNativeObjectField(nil), legacyEventNativeFields.fields...)
	if err := json.Unmarshal(event, &fixture.expected); err != nil {
		test.Fatal("L06 original typed event decoder rejected the fixture", err)
	}
	fixture.typedJSON, err = json.Marshal(fixture.expected)
	if err != nil || len(fixture.expected.Replacement) != shape.children || !fixture.expected.ReplacementComplete {
		test.Fatal("L06 original typed event lost its children or following field", err)
	}
	fixture.typedSHA = sha256.Sum256(fixture.typedJSON)
	return fixture
}

type legacyNativeEventsL06Reads struct {
	loads int
	bytes int64
	zero  int
	one   int
}

func legacyNativeEventsL06ReadCost(test testing.TB, buffer *legacySourceProofBuffer, from int) legacyNativeEventsL06Reads {
	test.Helper()
	var cost legacyNativeEventsL06Reads
	for _, read := range buffer.reads[from:] {
		switch {
		case read[0] == len(buffer.body) && read[1] == 0:
			cost.zero++
		case read[0] == len(buffer.body) && read[1] == 1:
			cost.one++
		case read[0] >= 0 && read[0] < len(buffer.body) && read[0]%ConversationManifestChunkBytes == 0 &&
			read[1] == min(ConversationManifestChunkBytes, len(buffer.body)-read[0]):
			cost.loads++
			cost.bytes += int64(read[1])
		default:
			test.Fatal("L06 bypassed complete authenticated source-part access", read)
		}
	}
	return cost
}

func legacyNativeEventsL06Producer(test testing.TB, fixture *legacyNativeEventsL06Fixture) (*legacySourceProofBuffer, LegacyByteSource, int) {
	test.Helper()
	ctx := context.Background()
	buffer := &legacySourceProofBuffer{body: bytes.Clone(fixture.raw), reads: make([][2]int, 0, 4098)}
	source := buffer.input()
	target := &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	producer, err := NewLegacyByteSourcePreparation(ctx, fixture.reference, source, target, &verifier)
	if err != nil || len(buffer.reads) != 0 {
		test.Fatal("L06 original source producer failed or read during construction", err)
	}
	for steps := 1; steps <= 64+4*len(source.Parts); steps++ {
		before := len(buffer.reads)
		progress, err := producer.Step(ctx)
		reads := legacyNativeEventsL06ReadCost(test, buffer, before)
		retained, retainedErr := producer.RetainedBytes()
		if err != nil || reads.loads > 2 || reads.bytes > 2*ConversationManifestChunkBytes || reads.zero+reads.one > 2 ||
			retainedErr != nil || retained <= 0 || retained >= 2<<20 {
			test.Fatal("L06 source producer escaped its original phase limits", steps, err, reads, retained, retainedErr)
		}
		if progress == LegacyByteSourcePending {
			continue
		}
		proof, resultErr := producer.Result()
		reads = legacyNativeEventsL06ReadCost(test, buffer, 0)
		if progress != LegacyByteSourceOriginalRequired || !producer.NormalizationEligible() || proof.Valid() ||
			!errors.Is(resultErr, ErrDocRangeIndexPending) || producer.actual != HashContent(fixture.raw) ||
			reads.loads != len(source.Parts) || reads.bytes != int64(len(fixture.raw)) || reads.zero != 1 || reads.one != 1 ||
			len(buffer.reads) != len(source.Parts)+2 || len(target.body) != 0 {
			test.Fatal("L06 source admission changed hash, EOF, access cost, or provisional proof status", progress, resultErr, reads)
		}
		for ordinal, part := range source.Parts {
			if buffer.reads[ordinal] != [2]int{ordinal * ConversationManifestChunkBytes, int(part.Bytes)} {
				test.Fatal("L06 source producer did not authenticate every original part in order", ordinal)
			}
		}
		buffer.reads = buffer.reads[:0]
		return buffer, source, steps
	}
	test.Fatal("L06 source producer exhausted its finite allowance")
	return nil, LegacyByteSource{}, 0
}

func legacyNativeEventsL06OldMetadata(test testing.TB) legacyNativeObjectFields {
	test.Helper()
	metadata := legacyEventNativeFields
	metadata.fields = append([]legacyNativeObjectField(nil), legacyEventNativeFields.fields...)
	changed := 0
	for index := range metadata.fields {
		if metadata.fields[index].eventList {
			if metadata.fields[index].name != "replacement" {
				test.Fatal("L06 event-list baseline unexpectedly covers another field", metadata.fields[index].name)
			}
			metadata.fields[index].eventList = false
			changed++
		}
	}
	if changed != 1 {
		test.Fatal("L06 requires the actual parent event-list metadata", changed)
	}
	return metadata
}

type legacyNativeEventsL06Cost struct {
	proofSteps int
	steps      int
	nextField  int
	maxWork    int
	maxLoads   int
	maxFields  int
	maxValues  int
	violations int
	work       int64
	semantic   int64
	reads      legacyNativeEventsL06Reads
}

func legacyNativeEventsL06Measure(test testing.TB, fixture *legacyNativeEventsL06Fixture, old bool, benchmark *testing.B, phase string) legacyNativeEventsL06Cost {
	test.Helper()
	ctx := context.Background()
	buffer, source, proofSteps := legacyNativeEventsL06Producer(test, fixture)
	metadata := legacyNativeEventsL06OldMetadata(test)
	if benchmark != nil && phase == "constructor" {
		benchmark.StartTimer()
	}
	native, err := newLegacyNormalizationNativeEvent()
	if err == nil {
		native.reset(fixture.eventStart, len(buffer.body))
		native.object.prefix = true
	}
	if benchmark != nil && phase == "constructor" {
		benchmark.StopTimer()
	}
	if err != nil || native == nil || native.object.metadata != &legacyEventNativeFields || len(buffer.reads) != 0 {
		test.Fatal("L06 actual native constructor failed or accessed the source", err)
	}
	defer native.releaseValues()
	if old {
		native.object.metadata = &metadata
	}
	var frame legacyJSONValueScanner
	cost := legacyNativeEventsL06Cost{proofSteps: proofSteps}
	for quantum := 0; quantum < 2048; quantum++ {
		before, position := len(buffer.reads), native.object.progressPosition()
		if benchmark != nil && phase == "decodeSteps" {
			benchmark.StartTimer()
		}
		input := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
		event, complete, failure := native.step(ctx, input, &frame)
		if benchmark != nil && phase == "decodeSteps" {
			benchmark.StopTimer()
		}
		cost.steps++
		reads := legacyNativeEventsL06ReadCost(test, buffer, before)
		if failure != nil || input.failure != nil || input.unbounded || reads.loads != input.loads || reads.zero != 0 || reads.one != 0 ||
			input.workBytes < 0 || input.fields < 0 || input.fields > native.object.fieldActionLimit() ||
			input.values < 0 || input.values > legacyNativeAnyValueActions || input.eventsRunning != nil || input.eventsPending != nil {
			test.Fatal("L06 actual native Step failed or lost shared input accounting", quantum, failure, input.failure, input.workBytes, input.loads, input.fields, input.values)
		}
		cost.work += int64(input.workBytes)
		cost.semantic += int64(input.semanticBytes)
		cost.maxWork, cost.maxLoads = max(cost.maxWork, input.workBytes), max(cost.maxLoads, input.loads)
		cost.maxFields, cost.maxValues = max(cost.maxFields, input.fields), max(cost.maxValues, input.values)
		if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
			cost.violations++
			if !old {
				test.Fatal("L06 actual event-list path escaped the unchanged 512 KiB/two-load quantum", quantum, input.workBytes, input.loads)
			}
		}
		if next := native.object.progressPosition(); next < position || next > fixture.eventStart+len(fixture.event) ||
			!old && next-position > input.workBytes {
			test.Fatal("L06 actual prefix cursor lost monotonic source progress", position, next, input.workBytes)
		}
		if native.decoded.ReplacementComplete && cost.nextField == 0 {
			cost.nextField = cost.steps
		}
		for _, page := range input.pages {
			clear(page)
		}
		if !complete {
			if native.canonicalReady {
				test.Fatal("L06 partial native decode became ready before its following field")
			}
			continue
		}
		typed, marshalErr := json.Marshal(event)
		if marshalErr != nil || !native.canonicalReady || cost.nextField == 0 || !event.ReplacementComplete ||
			native.object.position != fixture.eventStart+len(fixture.event) || !reflect.DeepEqual(event, fixture.expected) ||
			!bytes.Equal(typed, fixture.typedJSON) || sha256.Sum256(typed) != fixture.typedSHA {
			test.Fatal("L06 changed the original typed values, following field, final typed JSON, or SHA-256", marshalErr)
		}
		if old {
			owner, claimErr := native.takeCanonicalOwner(ctx)
			if owner != nil || !errors.Is(claimErr, ErrIntegrity) || !native.canonicalReady {
				test.Fatal("L06 local-metadata old baseline unexpectedly became owner-claim eligible", claimErr)
			}
		}
		if err := legacySourceEOF(ctx, source.Read, source.Bytes); err != nil {
			test.Fatal("L06 native source failed either final EOF probe", err)
		}
		cost.reads = legacyNativeEventsL06ReadCost(test, buffer, 0)
		visited := make([]bool, len(source.Parts))
		for _, read := range buffer.reads {
			if read[0] < source.Bytes {
				visited[read[0]/ConversationManifestChunkBytes] = true
			}
		}
		for ordinal, seen := range visited {
			if !seen {
				test.Fatal("L06 native decode omitted an authenticated source part", ordinal)
			}
		}
		if cost.reads.bytes < int64(source.Bytes) || cost.reads.zero != 1 || cost.reads.one != 1 ||
			!bytes.Equal(buffer.body, fixture.raw) || !bytes.Equal(buffer.body[fixture.eventStart:fixture.eventStart+len(fixture.event)], fixture.event) ||
			sha256.Sum256(buffer.body) != fixture.bytesSHA || sha256.Sum256(fixture.raw) != fixture.bytesSHA ||
			!reflect.DeepEqual(legacyEventNativeFields, fixture.metadata) {
			test.Fatal("L06 changed original bytes/SHA, EOF coverage, or global metadata", cost.reads)
		}
		return cost
	}
	test.Fatal("L06 actual native Step driver exhausted its finite allowance")
	return cost
}

func TestLegacyNativeEventsL06SourceCostParity(test *testing.T) {
	for _, shape := range legacyNativeEventsL06Cases() {
		test.Run(shape.name, func(test *testing.T) {
			fixture := legacyNativeEventsL06Source(test, shape)
			actual := legacyNativeEventsL06Measure(test, fixture, false, nil, "")
			old := legacyNativeEventsL06Measure(test, fixture, true, nil, "")
			if actual.proofSteps != old.proofSteps || actual.violations != 0 {
				test.Fatal("L06 A/B comparison used different source admission or relaxed the actual native gate", actual, old)
			}
			if len(fixture.event) > 2*ConversationManifestChunkBytes &&
				(actual.steps <= 1 || old.violations == 0 || old.maxWork <= ConversationManifestChunkBytes || old.maxLoads <= 2) {
				test.Fatal("L06 large fixture lost the original old-path atomic decode violation", actual, old)
			}
			test.Logf("source=%d event=%d; actual steps=%d reads=%d read-B=%d max-work=%d max-loads=%d next-field=%d; old steps=%d reads=%d read-B=%d max-work=%d max-loads=%d next-field=%d out-of-contract-steps=%d; raw-SHA=%x typed-SHA=%x; old baseline cannot claim an owner; no speedup or durable/publication proof is claimed", len(fixture.raw), len(fixture.event), actual.steps, actual.reads.loads, actual.reads.bytes, actual.maxWork, actual.maxLoads, actual.nextField, old.steps, old.reads.loads, old.reads.bytes, old.maxWork, old.maxLoads, old.nextField, old.violations, fixture.bytesSHA, fixture.typedSHA)
		})
	}
}

func BenchmarkLegacyNativeEventsL06SourceCost(benchmark *testing.B) {
	benchmark.StopTimer()
	benchmark.Log("Parent-only serial finite diagnostic: -benchtime=1x -count=1 -cpu=1. Exactly three source cases; N other than 1 is skipped. One sample is not a speedup, latency bound, percentile, or total-read-amplification improvement.")
	benchmark.Log("Both paths use the actual native constructor/reset/prefix and native.step driver with fresh shared input per call, identical authenticated part access, and the original source producer's hash/EOF normalization-admission gate. The old path changes only eventList flags in a detached local metadata fields slice; global metadata is never modified. It cannot takeCanonicalOwner and is not owner-claim evidence.")
	benchmark.Log("Separate timer phases: constructor includes newLegacyNormalizationNativeEvent/reset/prefix; decodeSteps includes fresh input and actual native.step calls, including authenticated source reads/copies/hashes. Source fixture/oracle generation, source producer Steps, metadata copying/switching, read/cursor/budget checks, page poisoning, original typed equality/final JSON/SHA checks, EOF checks, rejected old owner transfer, release, and timer bookkeeping are untimed. Untimed phases still complete on every leaf; this is not a whole-lifecycle benchmark or a plain json.Unmarshal speed comparison.")
	benchmark.Log("Actual decode retains the original 512 KiB/two-load and fields/values gates. Old atomic fallback violations are diagnostic metrics, not relaxed product bounds. Producer retention remains below 2 MiB. Canonical 64 KiB/1024, 16 MiB/64 MiB, 128k/setup30s, original depth, version/union/oracle, auth/incarnation/lease/CAS gates are not changed or certified here. Same-process parent/cursor resumption and call-local eventsRunning/eventsPending signals are neither byte hashes nor durable proofs. Canonical encoding, allocator latency, DB/network/publication, and durable restore are not timed or verified.")
	if ConversationManifestChunkBytes != 512<<10 || LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || LegacyNormalizationStepEvents != 64 {
		benchmark.Fatal("L06 requires the original source, canonical, and field action quanta")
	}
	for _, shape := range legacyNativeEventsL06Cases() {
		fixture := legacyNativeEventsL06Source(benchmark, shape)
		for _, old := range []bool{false, true} {
			path := "actualNative"
			if old {
				path = "localMetadataOldDecode-noOwner"
			}
			for _, phase := range []string{"constructor", "decodeSteps"} {
				benchmark.Run(fmt.Sprintf("%s/%s/%s", shape.name, path, phase), func(benchmark *testing.B) {
					benchmark.StopTimer()
					if benchmark.N != 1 {
						benchmark.Skip("finite L06 source cost requires -benchtime=1x; repeated calibration is disabled")
					}
					benchmark.ReportAllocs()
					benchmark.ResetTimer()
					cost := legacyNativeEventsL06Measure(benchmark, fixture, old, benchmark, phase)
					benchmark.ReportMetric(float64(len(fixture.raw)), "source-B")
					benchmark.ReportMetric(float64(len(fixture.event)), "event-B")
					benchmark.ReportMetric(float64(cost.proofSteps), "untimed-proof-steps/op")
					benchmark.ReportMetric(float64(len(fixture.raw)), "untimed-proof-read-B/op")
					benchmark.ReportMetric(2, "untimed-proof-EOF/op")
					benchmark.ReportMetric(float64(cost.steps), "decode-steps/op")
					benchmark.ReportMetric(float64(cost.reads.loads), "decode-part-loads/op")
					benchmark.ReportMetric(float64(cost.reads.bytes), "decode-read-B/op")
					benchmark.ReportMetric(float64(cost.reads.bytes)/float64(len(fixture.raw)), "decode-read-amplification")
					benchmark.ReportMetric(float64(cost.reads.bytes)+float64(len(fixture.raw)), "total-source-read-B/op")
					benchmark.ReportMetric(1+float64(cost.reads.bytes)/float64(len(fixture.raw)), "total-source-read-amplification")
					benchmark.ReportMetric(float64(cost.reads.loads+(len(fixture.raw)+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes), "total-source-part-loads/op")
					benchmark.ReportMetric(float64(2+cost.reads.zero+cost.reads.one), "total-source-EOF/op")
					benchmark.ReportMetric(float64(cost.work), "decode-work-B/op")
					benchmark.ReportMetric(float64(cost.semantic), "decode-semantic-B/op")
					benchmark.ReportMetric(float64(cost.maxWork), "max-work-B/step")
					benchmark.ReportMetric(float64(cost.maxLoads), "max-loads/step")
					benchmark.ReportMetric(float64(cost.maxFields), "max-fields/step")
					benchmark.ReportMetric(float64(cost.maxValues), "max-values/step")
					benchmark.ReportMetric(float64(cost.violations), "old-out-of-contract-steps/op")
					benchmark.ReportMetric(float64(cost.nextField), "next-field-step")
					benchmark.ReportMetric(float64(cost.reads.zero+cost.reads.one), "untimed-decode-EOF/op")
					benchmark.Logf("phase=%s raw-SHA=%x typed-SHA=%x; allocations cover selected timed intervals only, not fixture/source-producer storage or RSS; per-Step timer transitions perturb this single-shot diagnostic", phase, fixture.bytesSHA, fixture.typedSHA)
				})
			}
		}
	}
}
