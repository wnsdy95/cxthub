package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacySourceHeaderCostFixture struct {
	buffer       *legacySourceProofBuffer
	source       LegacyByteSource
	header       []byte
	typed        []byte
	envelope     CIREnvelope
	headerHash   ContentHash
	sourceSHA    [sha256.Size]byte
	typedSHA     [sha256.Size]byte
	manifestHeap int64
}

func legacySourceHeaderCostCases() []struct {
	name     string
	envelope CIREnvelope
} {
	escaped := strings.Repeat("\"\\\n\x00<&>\u2028\u2029한🙂", 8192)
	models := make([]string, 1025)
	for position := range models {
		models[position] = "same<&>\n한🙂"
	}
	models[0], models[512] = "", escaped
	return []struct {
		name     string
		envelope CIREnvelope
	}{
		{name: "small", envelope: CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, SourceModel: "small", Fidelity: FidelityFull}},
		{name: "ASCII512KiB", envelope: CIREnvelope{CIRVersion: "2", SourceModel: strings.Repeat("a", 512<<10), Fidelity: FidelityFull}},
		{name: "escape+SourceModels", envelope: CIREnvelope{CIRVersion: "2", SourceModel: escaped, SourceModels: models, ContextTokens: 128000, OutputTokens: 37, CompactionCount: 2}},
	}
}

func legacySourceHeaderCostSource(test testing.TB, envelope CIREnvelope) *legacySourceHeaderCostFixture {
	test.Helper()
	header, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	typed, err := json.Marshal(envelope)
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: AssembleCanonicalDoc(header, nil)}
	source := buffer.input()
	var manifest docRangeHeaderHeapEstimate
	manifest.allocation(int64(cap(source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	for _, part := range source.Parts {
		manifest.allocation(int64(len(part.Hash)), 1)
	}
	if manifest.err != nil || source.partBytes() != 512<<10 || source.Bytes > MaxFinalizedDocBytes {
		test.Fatal("header cost fixture changed the original source geometry or extent", manifest.err, source.Bytes)
	}
	return &legacySourceHeaderCostFixture{buffer: buffer, source: source, header: header, typed: typed, envelope: envelope,
		headerHash: HashContent(header), sourceSHA: sha256.Sum256(buffer.body), typedSHA: sha256.Sum256(typed), manifestHeap: manifest.bytes}
}

type legacySourceHeaderCost struct {
	steps         int
	phaseSteps    [4]int
	maxLoads      int
	maxRead       int64
	retained      int64
	materialized  int64
	canonicalHash ContentHash
	reads         legacyNativeEventsL06Reads
}

func legacySourceHeaderCostTyped(test testing.TB, fixture *legacySourceHeaderCostFixture, envelope CIREnvelope) {
	test.Helper()
	typed, err := json.Marshal(envelope)
	canonical, canonicalErr := canonicalJSON(envelope)
	if err != nil || canonicalErr != nil || !reflect.DeepEqual(envelope, fixture.envelope) ||
		!bytes.Equal(typed, fixture.typed) || sha256.Sum256(typed) != fixture.typedSHA ||
		!bytes.Equal(canonical, fixture.header) || HashContent(canonical) != fixture.headerHash {
		test.Fatal("old/new strict header changed typed values, exact typed/canonical bytes, or SHA-256", err, canonicalErr)
	}
}

func legacySourceHeaderCostMeasure(test testing.TB, fixture *legacySourceHeaderCostFixture, path string, benchmark *testing.B) legacySourceHeaderCost {
	test.Helper()
	ctx := context.Background()
	fixture.buffer.reads = nil
	var cost legacySourceHeaderCost
	switch path {
	case "oldStrictMaterialized":
		raw := bytes.Clone(fixture.header)
		if benchmark != nil {
			benchmark.StartTimer()
		}
		envelope, err := decodeCanonicalCIREnvelope(raw)
		if benchmark != nil {
			benchmark.StopTimer()
		}
		if err != nil {
			test.Fatal("original full Unmarshal/canonicalJSON/exact-byte oracle rejected the shared header", err)
		}
		legacySourceHeaderCostTyped(test, fixture, envelope)
		heap, err := legacyNativeValueHeap(ctx, reflect.ValueOf(envelope))
		if err != nil || heap.err != nil {
			test.Fatal("old strict return-boundary typed heap accounting failed", err, heap.err)
		}
		cost.materialized = int64(cap(raw))
		cost.retained = cost.materialized + heap.bytes
		cost.canonicalHash = HashContent(raw)
	case "newStrictFreshSource":
		if benchmark != nil {
			benchmark.StartTimer()
		}
		helper, err := newLegacySourceEnvelope(len(canonicalDocPrefix), len(canonicalDocPrefix)+len(fixture.header), fixture.headerHash)
		if benchmark != nil {
			benchmark.StopTimer()
		}
		if err != nil || helper == nil || len(fixture.buffer.reads) != 0 {
			test.Fatal("actual strict helper constructor failed or eagerly read the source", err)
		}
		defer helper.release()
		observe := func() {
			retained, err := helper.retained()
			if err != nil || retained <= 0 {
				test.Fatal("actual strict helper lost live retained accounting", retained, err)
			}
			cost.retained = max(cost.retained, retained)
		}
		observe()
		typedChecked, complete := false, false
		allowance := 128 + 32*len(fixture.source.Parts) + 16*((len(fixture.header)+LegacyCanonicalEncodingBytes-1)/LegacyCanonicalEncodingBytes)
		for quantum := 0; quantum < allowance; quantum++ {
			before := len(fixture.buffer.reads)
			if helper.phase > legacySourceEnvelopeEOF {
				test.Fatal("actual strict header reached an unexpected pending phase", helper.phase)
			}
			cost.phaseSteps[helper.phase]++
			if benchmark != nil {
				benchmark.StartTimer()
			}
			done, failure := helper.step(ctx, fixture.source)
			if benchmark != nil {
				benchmark.StopTimer()
			}
			cost.steps++
			reads := legacyGeometryCostReads(test, fixture.buffer, ConversationManifestChunkBytes, before)
			cost.maxLoads = max(cost.maxLoads, reads.loads)
			cost.maxRead = max(cost.maxRead, reads.bytes)
			if failure != nil || len(fixture.buffer.reads)-before > 2 || reads.loads > 2 || reads.bytes > 2*(512<<10) ||
				reads.zero+reads.one != 0 && reads.loads != 0 {
				test.Fatal("actual strict header escaped the original two-load/part/EOF boundary", quantum, failure, reads)
			}
			observe()
			if !typedChecked && helper.encoding != nil {
				legacySourceHeaderCostTyped(test, fixture, helper.encoding.envelope)
				typedChecked = true
			}
			if helper.phase == legacySourceEnvelopeEOF {
				cost.canonicalHash, err = legacySourceHashSum(ctx, helper.hashState)
				if err != nil || cost.canonicalHash != fixture.headerHash {
					test.Fatal("actual incremental encoder digest disagreed with the original exact-byte oracle", cost.canonicalHash, err)
				}
			}
			if done {
				complete = true
				break
			}
		}
		cost.reads = legacyGeometryCostReads(test, fixture.buffer, ConversationManifestChunkBytes, 0)
		visited := make([]bool, len(fixture.source.Parts))
		for _, read := range fixture.buffer.reads {
			if read[0] < fixture.source.Bytes {
				visited[read[0]/ConversationManifestChunkBytes] = true
			}
		}
		for ordinal, seen := range visited {
			if !seen {
				test.Fatal("actual strict header omitted an authenticated source part", ordinal)
			}
		}
		if !complete || !typedChecked || helper.version != fixture.envelope.CIRVersion || cost.reads.zero != 1 || cost.reads.one != 1 ||
			cost.reads.bytes < int64(fixture.source.Bytes) || len(fixture.header) > 512<<10 && cost.steps <= 1 {
			test.Fatal("actual strict header lost finite native/encoding/hash/EOF completion", complete, typedChecked, cost)
		}
	default:
		test.Fatal("unknown strict header cost path", path)
	}
	if cost.canonicalHash != fixture.headerHash || sha256.Sum256(fixture.buffer.body) != fixture.sourceSHA ||
		!bytes.Equal(fixture.buffer.body[len(canonicalDocPrefix):len(canonicalDocPrefix)+len(fixture.header)], fixture.header) {
		test.Fatal("strict header A/B changed the shared original source bytes or SHA-256")
	}
	return cost
}

func TestLegacySourceHeaderCostSingleShotParity(test *testing.T) {
	for _, shape := range legacySourceHeaderCostCases() {
		test.Run(shape.name, func(test *testing.T) {
			fixture := legacySourceHeaderCostSource(test, shape.envelope)
			for _, path := range []string{"oldStrictMaterialized", "newStrictFreshSource"} {
				cost := legacySourceHeaderCostMeasure(test, fixture, path, nil)
				test.Logf("%s source-B=%d header-B=%d typed-B=%d typed-SHA=%x source-SHA=%x canonical-hash=%s steps=%d native/init/encode-hash/EOF=%v part-loads=%d read-B=%d EOF=%d/%d max-loads/step=%d max-read-B/step=%d observed-state-retained-B=%d materialized-header-B=%d common-manifest-B=%d",
					path, fixture.source.Bytes, len(fixture.header), len(fixture.typed), fixture.typedSHA, fixture.sourceSHA, cost.canonicalHash,
					cost.steps, cost.phaseSteps, cost.reads.loads, cost.reads.bytes, cost.reads.zero, cost.reads.one, cost.maxLoads, cost.maxRead, cost.retained, cost.materialized, fixture.manifestHeap)
			}
		})
	}
}

func BenchmarkLegacySourceHeaderCostSingleShot(benchmark *testing.B) {
	benchmark.StopTimer()
	for name, expected := range map[string]string{"test.benchtime": "1x", "test.count": "1", "test.cpu": "1"} {
		configured := flag.Lookup(name)
		if configured == nil || configured.Value.String() != expected {
			benchmark.Skip("parent-only serial single-shot diagnostic requires -benchtime=1x -count=1 -cpu=1")
		}
	}
	if LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 || ConversationManifestChunkBytes != 512<<10 {
		benchmark.Fatal("strict header cost requires unchanged canonical/action/original-source quanta")
	}
	benchmark.Log("Exactly three same-header A/B cases, one operation per leaf. Old timed: original decodeCanonicalCIREnvelope full Unmarshal, version validation, full canonicalJSON, exact bytes.Equal. New timed: actual helper constructor and all native decode, encoder-init, incremental encoding/SHA-256, final EOF/digest steps, including fresh authenticated source reads/copies/hashes and read-history appends.")
	benchmark.Log("Untimed: fixture/body/materialized-old-input allocation, typed/canonical exact oracle and hashes, source manifest construction, retained/read/part-bound checks, release and timer bookkeeping. Original admission/frame/full-document-hash pass and PG publication are not executed or counted. Old starts from an already materialized header and performs no source reads/EOF; new includes those costs. Per-Step timer transitions perturb this single-shot diagnostic; B/op and allocs/op cover timed intervals only.")
	benchmark.Log("Old retained is raw-header backing plus typed heap at return, not transient canonicalJSON peak. New retained is maximum helper.retained observed at Step boundaries, not transient input/allocator/RSS peak. Common fixture/source/body/oracle/read-history and manifest retention are excluded and manifest is reported separately. No speedup, RSS, 512 MiB capacity, p95, whole-PG performance, durable restore, or R2-R5 completion claim; SHA comparison uses the existing content-address collision-resistance assumption and independent exact bytes remain the test oracle.")
	for _, shape := range legacySourceHeaderCostCases() {
		fixture := legacySourceHeaderCostSource(benchmark, shape.envelope)
		for _, path := range []string{"oldStrictMaterialized", "newStrictFreshSource"} {
			benchmark.Run(shape.name+"/"+path, func(benchmark *testing.B) {
				benchmark.StopTimer()
				if benchmark.N != 1 {
					benchmark.Skip("strict header cost disables repeated calibration; use -benchtime=1x")
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				cost := legacySourceHeaderCostMeasure(benchmark, fixture, path, benchmark)
				benchmark.ReportMetric(float64(len(fixture.header)), "header-B")
				benchmark.ReportMetric(float64(fixture.source.Bytes), "source-B")
				benchmark.ReportMetric(float64(len(fixture.typed)), "typed-B")
				benchmark.ReportMetric(float64(cost.steps), "helper-steps/op")
				for phase, name := range []string{"native", "encoder-init", "encode-hash", "EOF"} {
					benchmark.ReportMetric(float64(cost.phaseSteps[phase]), name+"-steps/op")
				}
				benchmark.ReportMetric(float64(cost.reads.loads), "part-loads/op")
				benchmark.ReportMetric(float64(cost.reads.bytes), "source-read-B/op")
				benchmark.ReportMetric(float64(cost.reads.zero+cost.reads.one), "EOF-probes/op")
				benchmark.ReportMetric(float64(cost.maxLoads), "max-part-loads/step")
				benchmark.ReportMetric(float64(cost.maxRead), "max-source-read-B/step")
				benchmark.ReportMetric(float64(cost.retained), "observed-state-retained-B")
				benchmark.ReportMetric(float64(cost.materialized), "untimed-materialized-header-B")
				benchmark.ReportMetric(float64(fixture.manifestHeap), "untimed-common-manifest-B")
				benchmark.Logf("canonical-hash=%s shared source-SHA=%x typed-SHA=%x; exact typed/canonical oracle checked outside timing", cost.canonicalHash, fixture.sourceSHA, fixture.typedSHA)
			})
		}
	}
}
