package domain

import (
	"context"
	"errors"
	"io"
	"testing"
)

const legacyNativePublicRejectCostPrefix = `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","seq":1,"call_id":"huge-number","output":`
const legacyNativePublicRejectCostSuffix = `}]}`

type legacyNativePublicRejectCostSource struct {
	bytes           int
	page            []byte
	parts           []ConversationManifestChunk
	visited         []bool
	readBytes       int64
	coveredBytes    int64
	loads           int
	zeroEOF         int
	oneEOF          int
	unexpectedReads int
}

func newLegacyNativePublicRejectCostSource(test testing.TB, extent int) *legacyNativePublicRejectCostSource {
	test.Helper()
	if extent < len(legacyNativePublicRejectCostPrefix)+310+len(legacyNativePublicRejectCostSuffix) || extent > MaxFinalizedDocBytes {
		test.Fatal("generated overflow document has an invalid extent", extent)
	}
	count := (extent + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	source := &legacyNativePublicRejectCostSource{
		bytes: extent, page: make([]byte, ConversationManifestChunkBytes),
		parts: make([]ConversationManifestChunk, count), visited: make([]bool, count),
	}
	var total int64
	for ordinal := range source.parts {
		offset := ordinal * ConversationManifestChunkBytes
		length := min(ConversationManifestChunkBytes, extent-offset)
		body, err := source.generate(context.Background(), offset, length)
		if err != nil || len(body) != length {
			test.Fatal("could not generate a complete source part", ordinal, len(body), length, err)
		}
		source.parts[ordinal] = ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(length)}
		total += int64(length)
	}
	if total != int64(extent) || cap(source.page) > 512<<10 ||
		len(legacyNativePublicRejectCostPrefix)+len(legacyNativePublicRejectCostSuffix) > 64<<10 {
		test.Fatal("generated source changed its exact extent or bounded backing", total, extent, cap(source.page))
	}
	return source
}

func (source *legacyNativePublicRejectCostSource) generate(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > len(source.page) {
		return nil, ErrIntegrity
	}
	if offset > source.bytes {
		return nil, io.EOF
	}
	available := min(length, source.bytes-offset)
	body := source.page[:available:available]
	for index := range body {
		if index%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		body[index] = '9'
	}
	if offset < len(legacyNativePublicRejectCostPrefix) {
		copy(body, legacyNativePublicRejectCostPrefix[offset:])
	}
	suffixStart := source.bytes - len(legacyNativePublicRejectCostSuffix)
	if from := max(offset, suffixStart); from < offset+available {
		copy(body[from-offset:], legacyNativePublicRejectCostSuffix[from-suffixStart:])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if available < length {
		return body, io.EOF
	}
	return body, nil
}

func (source *legacyNativePublicRejectCostSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	body, err := source.generate(ctx, offset, length)
	source.readBytes += int64(len(body))
	switch {
	case offset == source.bytes && length == 0 && err == nil && len(body) == 0:
		source.zeroEOF++
	case offset == source.bytes && length == 1 && errors.Is(err, io.EOF) && len(body) == 0:
		source.oneEOF++
	case offset >= 0 && offset < source.bytes && offset%ConversationManifestChunkBytes == 0 &&
		length == min(ConversationManifestChunkBytes, source.bytes-offset) && len(body) == length && err == nil:
		ordinal := offset / ConversationManifestChunkBytes
		source.loads++
		if !source.visited[ordinal] {
			source.visited[ordinal] = true
			source.coveredBytes += int64(length)
		}
	default:
		source.unexpectedReads++
	}
	return body, err
}

func (source *legacyNativePublicRejectCostSource) reset() {
	clear(source.visited)
	source.readBytes, source.coveredBytes = 0, 0
	source.loads, source.zeroEOF, source.oneEOF, source.unexpectedReads = 0, 0, 0, 0
}

type legacyNativePublicRejectCost struct {
	steps            int
	maxStepLoads     int
	maxStepReadBytes int64
	maxRetained      int64
}

func legacyNativePublicRejectCostMeasure(test testing.TB, source *legacyNativePublicRejectCostSource) legacyNativePublicRejectCost {
	test.Helper()
	source.reset()
	ctx := context.Background()
	decoder, err := NewLegacyNormalizationDecoder(ctx, LegacyByteSource{Bytes: source.bytes, Parts: source.parts, Read: source.Read})
	if err != nil || decoder == nil || source.loads != 0 || source.readBytes != 0 || source.zeroEOF != 0 || source.oneEOF != 0 || source.unexpectedReads != 0 {
		test.Fatal("public decoder constructor failed or eagerly read the generated source", err)
	}
	var cost legacyNativePublicRejectCost
	observe := func() {
		retained, retainedErr := decoder.RetainedBytes()
		if retainedErr != nil || retained <= 0 || retained > 16<<20 {
			test.Fatal("numeric rejection lost retained accounting or exceeded the parked heap gate", retained, retainedErr)
		}
		cost.maxRetained = max(cost.maxRetained, retained)
	}
	observe()
	for allowance := 64 + 32*len(source.parts); cost.steps < allowance; {
		beforeLoads, beforeBytes := source.loads, source.readBytes
		step, failure := decoder.Step(ctx)
		cost.steps++
		loads, readBytes := source.loads-beforeLoads, source.readBytes-beforeBytes
		cost.maxStepLoads = max(cost.maxStepLoads, loads)
		cost.maxStepReadBytes = max(cost.maxStepReadBytes, readBytes)
		if loads > 2 || readBytes > 2*ConversationManifestChunkBytes || source.unexpectedReads != 0 {
			test.Fatal("public rejection escaped authenticated bounded part reads", cost.steps, loads, readBytes, source.unexpectedReads)
		}
		observe()
		if step.Complete || len(step.Events) != 0 || len(step.Envelope) != 0 || step.Count != 0 {
			test.Fatal("overflow document exposed a provisional event or successful result", cost.steps)
		}
		if failure == nil {
			continue
		}
		if failure != ErrIntegrity || step.Reset {
			test.Fatalf("public overflow did not end with an atomic ErrIntegrity: type=%T integrity=%t private=%t", failure, errors.Is(failure, ErrIntegrity), errors.Is(failure, ErrPreparedDocRangeObject))
		}
		if source.coveredBytes != int64(source.bytes) || source.readBytes < int64(source.bytes) || source.readBytes > 8*int64(source.bytes)+2 ||
			source.zeroEOF == 0 || source.oneEOF == 0 || cost.steps <= len(source.parts) {
			test.Fatal("public rejection did not consume the full source and EOF within linear work", source.bytes, source.coveredBytes, source.readBytes, source.zeroEOF, source.oneEOF, cost.steps)
		}
		for ordinal, visited := range source.visited {
			if !visited {
				test.Fatal("public rejection omitted an authenticated source part", ordinal)
			}
		}
		return cost
	}
	test.Fatal("public rejection exhausted its finite per-part step allowance", cost.steps, source.readBytes)
	return cost
}

func TestLegacyNativePublicRejectCostGeneratedSourceOffsets(test *testing.T) {
	const extent = 2*ConversationManifestChunkBytes + 37
	source := newLegacyNativePublicRejectCostSource(test, extent)
	check := func(offset, length, expectedLength int, expectedError error) {
		test.Helper()
		body, err := source.Read(context.Background(), offset, length)
		if len(body) != expectedLength || !errors.Is(err, expectedError) {
			test.Fatal("generated source changed requested span or EOF", offset, length, len(body), expectedLength, err, expectedError)
		}
		for index, current := range body {
			position, expected := offset+index, byte('9')
			switch {
			case position < len(legacyNativePublicRejectCostPrefix):
				expected = legacyNativePublicRejectCostPrefix[position]
			case position >= extent-len(legacyNativePublicRejectCostSuffix):
				expected = legacyNativePublicRejectCostSuffix[position-(extent-len(legacyNativePublicRejectCostSuffix))]
			}
			if current != expected {
				test.Fatal("generated source changed an absolute byte", offset, length, position, current, expected)
			}
		}
	}
	for ordinal, part := range source.parts {
		offset := ordinal * ConversationManifestChunkBytes
		check(offset, int(part.Bytes), int(part.Bytes), nil)
		body, err := source.Read(context.Background(), offset, int(part.Bytes))
		if err != nil || HashContent(body) != part.Hash {
			test.Fatal("generated source metadata did not authenticate its real bytes", ordinal, err)
		}
	}
	for _, offset := range []int{0, 1, len(legacyNativePublicRejectCostPrefix) - 1, len(legacyNativePublicRejectCostPrefix),
		ConversationManifestChunkBytes - 17, ConversationManifestChunkBytes + 19,
		extent - len(legacyNativePublicRejectCostSuffix) - 13, extent - len(legacyNativePublicRejectCostSuffix), extent - 1, extent} {
		check(offset, 0, 0, nil)
		for _, length := range []int{1, 7, 41, canonicalCheckBytes, ConversationManifestChunkBytes} {
			expectedLength := min(length, extent-offset)
			var expectedError error
			if expectedLength < length {
				expectedError = io.EOF
			}
			check(offset, length, expectedLength, expectedError)
		}
	}
	check(-1, 1, 0, ErrIntegrity)
	check(0, -1, 0, ErrIntegrity)
	check(0, ConversationManifestChunkBytes+1, 0, ErrIntegrity)
	check(extent+1, 0, 0, io.EOF)
	check(extent+1, 1, 0, io.EOF)
	check(int(^uint(0)>>1), 1, 0, io.EOF)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if body, err := source.Read(ctx, 0, ConversationManifestChunkBytes); len(body) != 0 || !errors.Is(err, context.Canceled) {
		test.Fatal("generated source ignored the current canceled context", len(body), err)
	}
}

func TestLegacyNativePublicRejectCost16MiB(test *testing.T) {
	const extent = 16 << 20
	source := newLegacyNativePublicRejectCostSource(test, extent)
	cost := legacyNativePublicRejectCostMeasure(test, source)
	test.Logf("sourcebytes=%d steps=%d readbytes=%d partloads=%d eof=%d/%d maxretained=%d maxloads/step=%d maxreadbytes/step=%d",
		source.bytes, cost.steps, source.readBytes, source.loads, source.zeroEOF, source.oneEOF, cost.maxRetained, cost.maxStepLoads, cost.maxStepReadBytes)
}

func BenchmarkLegacyNativePublicRejectMax512MiB(benchmark *testing.B) {
	if MaxFinalizedDocBytes != 512<<20 || ConversationManifestChunkBytes != 512<<10 {
		benchmark.Fatal("maximum rejection cost requires the real 512 MiB document and 512 KiB source parts")
	}
	source := newLegacyNativePublicRejectCostSource(benchmark, MaxFinalizedDocBytes)
	benchmark.ReportAllocs()
	benchmark.SetBytes(int64(source.bytes))
	benchmark.ResetTimer()
	var totalSteps, totalLoads, totalEOF int64
	var totalReadBytes, maxRetained, maxStepReadBytes int64
	var maxStepLoads int
	for operation := 0; operation < benchmark.N; operation++ {
		cost := legacyNativePublicRejectCostMeasure(benchmark, source)
		totalSteps += int64(cost.steps)
		totalLoads += int64(source.loads)
		totalEOF += int64(source.zeroEOF + source.oneEOF)
		totalReadBytes += source.readBytes
		maxRetained = max(maxRetained, cost.maxRetained)
		maxStepLoads = max(maxStepLoads, cost.maxStepLoads)
		maxStepReadBytes = max(maxStepReadBytes, cost.maxStepReadBytes)
	}
	benchmark.StopTimer()
	benchmark.ReportMetric(float64(totalSteps)/float64(benchmark.N), "steps/op")
	benchmark.ReportMetric(float64(totalReadBytes)/float64(benchmark.N), "readbytes/op")
	benchmark.ReportMetric(float64(totalLoads)/float64(benchmark.N), "part-loads/op")
	benchmark.ReportMetric(float64(totalEOF)/float64(benchmark.N), "eof-probes/op")
	benchmark.ReportMetric(float64(source.bytes), "sourcebytes/op")
	benchmark.ReportMetric(float64(maxRetained), "max-retained-B")
	benchmark.ReportMetric(float64(maxStepLoads), "max-part-loads/step")
	benchmark.ReportMetric(float64(maxStepReadBytes), "max-readbytes/step")
}
