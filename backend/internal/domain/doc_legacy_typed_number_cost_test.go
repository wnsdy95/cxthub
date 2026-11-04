package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"testing"
)

const legacyTypedNumberCostPrefix = `{"envelope":{"cir_version":"2"},"events":[{"seq":7,"role":"user","provider_metadata":{"turn_id":"number-cost","create_time":`
const legacyTypedNumberCostSuffix = `},"kind":"turn"}]}`
const legacyTypedNumberCostNumberPrefix = "-0."
const legacyTypedNumberCostNumberSuffix = "00E+0003"
const legacyTypedNumberCostEnvelope = `{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""}`
const legacyTypedNumberCostCanonicalPrefix = `{"envelope":` + legacyTypedNumberCostEnvelope + `,"events":[{"kind":"turn","provider_metadata":{"create_time":`
const legacyTypedNumberCostCanonicalSuffix = `,"turn_id":"number-cost"},"role":"user","seq":7}]}`

type legacyTypedNumberCostSource struct {
	bytes           int
	numberBytes     int
	prefix          string
	suffix          string
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

func legacyTypedNumberCostSourceNew(test testing.TB, numberBytes int, quoted bool) *legacyTypedNumberCostSource {
	test.Helper()
	if numberBytes <= len(legacyTypedNumberCostNumberPrefix)+len(legacyTypedNumberCostNumberSuffix) || numberBytes > 16<<20 {
		test.Fatal("accepted-number cost fixture requires a positive bounded mantissa, at most 16 MiB", numberBytes)
	}
	source := &legacyTypedNumberCostSource{
		numberBytes: numberBytes,
		prefix:      legacyTypedNumberCostPrefix,
		suffix:      legacyTypedNumberCostSuffix,
		page:        make([]byte, ConversationManifestChunkBytes),
	}
	if quoted {
		source.prefix += `"`
		source.suffix = `"` + source.suffix
	}
	source.bytes = len(source.prefix) + numberBytes + len(source.suffix)
	if source.bytes > MaxFinalizedDocBytes || cap(source.page) > 512<<10 {
		test.Fatal("accepted-number fixture escaped the original document or source-page bounds", source.bytes, cap(source.page))
	}
	count := (source.bytes + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	source.parts = make([]ConversationManifestChunk, count)
	source.visited = make([]bool, count)
	for ordinal := range source.parts {
		offset := ordinal * ConversationManifestChunkBytes
		length := min(ConversationManifestChunkBytes, source.bytes-offset)
		body, err := source.generate(context.Background(), offset, length)
		if err != nil || len(body) != length {
			test.Fatal("could not generate authenticated accepted-number metadata", ordinal, len(body), err)
		}
		source.parts[ordinal] = ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(length)}
	}
	return source
}

func legacyTypedNumberCostNumberByte(position, extent int) byte {
	if position < len(legacyTypedNumberCostNumberPrefix) {
		return legacyTypedNumberCostNumberPrefix[position]
	}
	if position >= extent-len(legacyTypedNumberCostNumberSuffix) {
		return legacyTypedNumberCostNumberSuffix[position-(extent-len(legacyTypedNumberCostNumberSuffix))]
	}
	return "1234567890"[(position-len(legacyTypedNumberCostNumberPrefix))%10]
}

func (source *legacyTypedNumberCostSource) generate(ctx context.Context, offset, length int) ([]byte, error) {
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
		position := offset + index
		switch {
		case position < len(source.prefix):
			body[index] = source.prefix[position]
		case position < len(source.prefix)+source.numberBytes:
			body[index] = legacyTypedNumberCostNumberByte(position-len(source.prefix), source.numberBytes)
		default:
			body[index] = source.suffix[position-len(source.prefix)-source.numberBytes]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if available < length {
		return body, io.EOF
	}
	return body, nil
}

func (source *legacyTypedNumberCostSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
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

func (source *legacyTypedNumberCostSource) reset() {
	clear(source.visited)
	source.readBytes, source.coveredBytes = 0, 0
	source.loads, source.zeroEOF, source.oneEOF, source.unexpectedReads = 0, 0, 0, 0
}

type legacyTypedNumberCostExpected struct {
	digest [sha256.Size]byte
	bytes  int64
}

func legacyTypedNumberCostExpectedNew(numberBytes int) legacyTypedNumberCostExpected {
	digest := sha256.New()
	_, _ = io.WriteString(digest, legacyTypedNumberCostCanonicalPrefix)
	var page [LegacyCanonicalEncodingBytes]byte
	for offset := 0; offset < numberBytes; {
		length := min(len(page), numberBytes-offset)
		for index := range page[:length] {
			page[index] = legacyTypedNumberCostNumberByte(offset+index, numberBytes)
		}
		_, _ = digest.Write(page[:length])
		offset += length
	}
	_, _ = io.WriteString(digest, legacyTypedNumberCostCanonicalSuffix)
	expected := legacyTypedNumberCostExpected{bytes: int64(len(legacyTypedNumberCostCanonicalPrefix) + numberBytes + len(legacyTypedNumberCostCanonicalSuffix))}
	digest.Sum(expected.digest[:0])
	return expected
}

type legacyTypedNumberCost struct {
	decodeSteps            int
	encodingSteps          int
	maxStepLoads           int
	maxStepReadBytes       int64
	maxDecoderRetained     int64
	maxEncodingRetained    int64
	maxLiveRetained        int64
	maxChunkBytes          int
	canonicalBytes         int64
	decodeAllocatedBytes   uint64
	encodingAllocatedBytes uint64
	decodeAllocations      uint64
	encodingAllocations    uint64
}

func legacyTypedNumberCostSample(previous *runtime.MemStats, allocatedBytes, allocations *uint64) {
	var current runtime.MemStats
	runtime.ReadMemStats(&current)
	*allocatedBytes += current.TotalAlloc - previous.TotalAlloc
	*allocations += current.Mallocs - previous.Mallocs
	*previous = current
}

func legacyTypedNumberCostMeasure(test testing.TB, source *legacyTypedNumberCostSource, expected legacyTypedNumberCostExpected, allocations bool) legacyTypedNumberCost {
	test.Helper()
	source.reset()
	var cost legacyTypedNumberCost
	var sample runtime.MemStats
	if allocations {
		runtime.ReadMemStats(&sample)
	}
	ctx := context.Background()
	decoder, err := NewLegacyNormalizationDecoder(ctx, LegacyByteSource{Bytes: source.bytes, Parts: source.parts, Read: source.Read})
	if err != nil || decoder == nil || source.loads != 0 || source.readBytes != 0 || source.zeroEOF != 0 || source.oneEOF != 0 || source.unexpectedReads != 0 {
		test.Fatal("accepted-number decoder failed or eagerly read generated payloads", err)
	}
	observe := func(encoding *LegacyCanonicalEncoding) {
		decoderRetained, retainedErr := decoder.RetainedBytes()
		if retainedErr != nil || decoderRetained <= 0 {
			test.Fatal("accepted-number decoder lost retained accounting", decoderRetained, retainedErr)
		}
		var encodingRetained int64
		if encoding != nil {
			encodingRetained, retainedErr = encoding.RetainedBytes()
			if retainedErr != nil || encodingRetained <= 0 {
				test.Fatal("accepted-number canonical encoder lost retained accounting", encodingRetained, retainedErr)
			}
		}
		cost.maxDecoderRetained = max(cost.maxDecoderRetained, decoderRetained)
		cost.maxEncodingRetained = max(cost.maxEncodingRetained, encodingRetained)
		cost.maxLiveRetained = max(cost.maxLiveRetained, decoderRetained+encodingRetained)
	}
	observe(nil)
	digest := sha256.New()
	prefix := `{"envelope":` + legacyTypedNumberCostEnvelope + `,"events":[`
	_, _ = io.WriteString(digest, prefix)
	cost.canonicalBytes = int64(len(prefix))
	seen := 0
	resets := 0
	for allowance := 64 + 32*len(source.parts); cost.decodeSteps < allowance; {
		beforeLoads, beforeBytes := source.loads, source.readBytes
		stepContext, cancelStep := context.WithCancel(ctx)
		step, failure := decoder.Step(stepContext)
		cancelStep()
		cost.decodeSteps++
		loads, readBytes := source.loads-beforeLoads, source.readBytes-beforeBytes
		cost.maxStepLoads = max(cost.maxStepLoads, loads)
		cost.maxStepReadBytes = max(cost.maxStepReadBytes, readBytes)
		if failure != nil {
			test.Fatalf("authenticated accepted provider Number failed: type=%T integrity=%t private=%t", failure, errors.Is(failure, ErrIntegrity), errors.Is(failure, ErrPreparedDocRangeObject))
		}
		if loads > 2 || readBytes > 2*ConversationManifestChunkBytes || source.unexpectedReads != 0 {
			test.Fatal("accepted provider Number bypassed bounded authenticated reads", cost.decodeSteps, loads, readBytes, source.unexpectedReads)
		}
		if step.Reset {
			resets++
			if resets != 1 || seen != 0 || len(step.Events) != 0 || step.Complete {
				test.Fatal("single-event source reset after emitting an owned encoding", resets, seen)
			}
		}
		if len(step.Events) > 1 || !step.Complete && (step.Count != 0 || len(step.Envelope) != 0) {
			test.Fatal("accepted provider Number changed the single-event provisional contract", cost.decodeSteps)
		}
		observe(nil)
		for _, event := range step.Events {
			seen++
			if seen != 1 || event.Sequence != 7 || event.Encoding == nil {
				test.Fatal("accepted provider Number lost its single owned encoding or sequence", seen, event.Sequence)
			}
			root, rootOK := event.Encoding.root.(map[string]any)
			metadata, metadataOK := root["provider_metadata"].(map[string]any)
			number, numberOK := metadata["create_time"].(*json.Number)
			if !rootOK || !metadataOK || !numberOK || number == nil || len(*number) != source.numberBytes {
				test.Fatal("provider create_time stopped being an exact typed Number pointer")
			}
			observe(event.Encoding)
			if cost.maxEncodingRetained < int64(source.numberBytes) {
				test.Fatal("accepted Number's owned lexical backing was not accounted", cost.maxEncodingRetained, source.numberBytes)
			}
			if allocations {
				legacyTypedNumberCostSample(&sample, &cost.decodeAllocatedBytes, &cost.decodeAllocations)
			}
			beforeLoads, beforeBytes = source.loads, source.readBytes
			beforeZeroEOF, beforeOneEOF := source.zeroEOF, source.oneEOF
			complete := false
			encodingAllowance := 64 + 4*((source.numberBytes+LegacyCanonicalEncodingBytes-1)/LegacyCanonicalEncodingBytes)
			for quantum := 0; quantum < encodingAllowance; quantum++ {
				encodingContext, cancelEncoding := context.WithCancel(ctx)
				chunk, done, encodingErr := event.Encoding.Next(encodingContext)
				cancelEncoding()
				cost.encodingSteps++
				if encodingErr != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 {
					test.Fatal("accepted provider Number escaped bounded canonical encoding", len(chunk), cap(chunk), encodingErr)
				}
				cost.maxChunkBytes = max(cost.maxChunkBytes, len(chunk))
				_, _ = digest.Write(chunk)
				cost.canonicalBytes += int64(len(chunk))
				observe(event.Encoding)
				if done {
					complete = true
					break
				}
			}
			if !complete || source.loads != beforeLoads || source.readBytes != beforeBytes || source.zeroEOF != beforeZeroEOF || source.oneEOF != beforeOneEOF {
				test.Fatal("canonical Number encoding failed to finish independently of source reads", cost.encodingSteps)
			}
			if allocations {
				legacyTypedNumberCostSample(&sample, &cost.encodingAllocatedBytes, &cost.encodingAllocations)
			}
		}
		if !step.Complete {
			continue
		}
		if seen != 1 || resets != 1 || step.Count != 1 || !bytes.Equal(step.Envelope, []byte(legacyTypedNumberCostEnvelope)) {
			test.Fatal("accepted provider Number lost its final envelope or event count", seen, step.Count)
		}
		if source.coveredBytes != int64(source.bytes) || source.readBytes < int64(source.bytes) || source.readBytes > 8*int64(source.bytes)+2 || source.zeroEOF == 0 || source.oneEOF == 0 {
			test.Fatal("accepted provider Number did not consume authenticated pages through both EOF probes within linear reads", source.coveredBytes, source.readBytes, source.zeroEOF, source.oneEOF)
		}
		for ordinal, visited := range source.visited {
			if !visited {
				test.Fatal("accepted provider Number omitted a source page", ordinal)
			}
		}
		_, _ = io.WriteString(digest, `]}`)
		cost.canonicalBytes += 2
		var actual [sha256.Size]byte
		digest.Sum(actual[:0])
		if actual != expected.digest || cost.canonicalBytes != expected.bytes {
			test.Fatal("streamed canonical document changed Number spelling, key order, or exact extent", cost.canonicalBytes, expected.bytes)
		}
		if source.numberBytes > ConversationManifestChunkBytes && (cost.decodeSteps <= len(source.parts) || cost.encodingSteps <= source.numberBytes/LegacyCanonicalEncodingBytes) {
			test.Fatal("accepted provider Number did not yield across its real extent", cost.decodeSteps, cost.encodingSteps)
		}
		if allocations {
			legacyTypedNumberCostSample(&sample, &cost.decodeAllocatedBytes, &cost.decodeAllocations)
		}
		return cost
	}
	test.Fatal("accepted provider Number exhausted its finite per-page step allowance", cost.decodeSteps, source.readBytes)
	return cost
}

func TestLegacyTypedNumberCostSmallOracle(test *testing.T) {
	const number = "-0.123456789012345678900E+0003"
	for _, spelling := range []struct {
		name   string
		quoted bool
	}{
		{name: "raw"},
		{name: "quoted", quoted: true},
	} {
		test.Run(spelling.name, func(test *testing.T) {
			source := legacyTypedNumberCostSourceNew(test, len(number), spelling.quoted)
			raw, err := source.generate(context.Background(), 0, source.bytes)
			var document CIRDocument
			if err != nil || json.Unmarshal(raw, &document) != nil || len(document.Events) != 1 || document.Events[0].ProviderMetadata == nil || document.Events[0].ProviderMetadata.CreateTime == nil || string(*document.Events[0].ProviderMetadata.CreateTime) != number {
				test.Fatal("small generated Number did not match independent native spelling", err)
			}
			canonical, err := CanonicalBytes(document)
			expected := legacyTypedNumberCostExpectedNew(len(number))
			if err != nil || sha256.Sum256(canonical) != expected.digest || int64(len(canonical)) != expected.bytes {
				test.Fatal("small native equality oracle disagreed with the generated streaming expectation", err)
			}
			legacyTypedNumberCostMeasure(test, source, expected, false)
		})
	}
}

func TestLegacyTypedNumberCostAccepted4MiB(test *testing.T) {
	for _, spelling := range []struct {
		name   string
		quoted bool
	}{
		{name: "raw"},
		{name: "quoted", quoted: true},
	} {
		test.Run(spelling.name, func(test *testing.T) {
			source := legacyTypedNumberCostSourceNew(test, 4<<20, spelling.quoted)
			cost := legacyTypedNumberCostMeasure(test, source, legacyTypedNumberCostExpectedNew(source.numberBytes), false)
			test.Logf("numberbytes=%d sourcebytes=%d decode-steps=%d encode-steps=%d readbytes=%d partloads=%d eof=%d/%d decoder-retained-B=%d encoder-retained-B=%d live-retained-B=%d maxloads/step=%d maxreadbytes/step=%d maxchunkbytes=%d canonicalbytes=%d",
				source.numberBytes, source.bytes, cost.decodeSteps, cost.encodingSteps, source.readBytes, source.loads, source.zeroEOF, source.oneEOF,
				cost.maxDecoderRetained, cost.maxEncodingRetained, cost.maxLiveRetained, cost.maxStepLoads, cost.maxStepReadBytes, cost.maxChunkBytes, cost.canonicalBytes)
		})
	}
}

func BenchmarkLegacyTypedNumberCostAccepted16MiB(benchmark *testing.B) {
	for _, spelling := range []struct {
		name   string
		quoted bool
	}{
		{name: "raw"},
		{name: "quoted", quoted: true},
	} {
		benchmark.Run(spelling.name, func(benchmark *testing.B) {
			benchmark.StopTimer()
			source := legacyTypedNumberCostSourceNew(benchmark, 16<<20, spelling.quoted)
			expected := legacyTypedNumberCostExpectedNew(source.numberBytes)
			benchmark.Log("Optional parent-only serial diagnostic: invoke without -race, with -benchtime=1x. Source-page allocation, initial metadata hashes, and expected streaming digest construction are outside the timer and allocation baseline.")
			benchmark.Log("Phase allocation deltas are process-wide MemStats diagnostics including harness work; ns/op includes sampling. Retained snapshots are separate from allocated bytes. The native builder owns the accepted value: this is not a 16 MiB parking or 512 MiB server-memory claim.")
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(source.bytes))
			var total legacyTypedNumberCost
			var totalReadBytes, totalLoads, totalEOF int64
			benchmark.ResetTimer()
			benchmark.StartTimer()
			for operation := 0; operation < benchmark.N; operation++ {
				cost := legacyTypedNumberCostMeasure(benchmark, source, expected, true)
				total.decodeSteps += cost.decodeSteps
				total.encodingSteps += cost.encodingSteps
				total.decodeAllocatedBytes += cost.decodeAllocatedBytes
				total.encodingAllocatedBytes += cost.encodingAllocatedBytes
				total.decodeAllocations += cost.decodeAllocations
				total.encodingAllocations += cost.encodingAllocations
				total.maxDecoderRetained = max(total.maxDecoderRetained, cost.maxDecoderRetained)
				total.maxEncodingRetained = max(total.maxEncodingRetained, cost.maxEncodingRetained)
				total.maxLiveRetained = max(total.maxLiveRetained, cost.maxLiveRetained)
				total.maxStepLoads = max(total.maxStepLoads, cost.maxStepLoads)
				total.maxStepReadBytes = max(total.maxStepReadBytes, cost.maxStepReadBytes)
				total.maxChunkBytes = max(total.maxChunkBytes, cost.maxChunkBytes)
				totalReadBytes += source.readBytes
				totalLoads += int64(source.loads)
				totalEOF += int64(source.zeroEOF + source.oneEOF)
			}
			benchmark.StopTimer()
			operations := float64(benchmark.N)
			benchmark.ReportMetric(float64(total.decodeSteps)/operations, "decode-steps/op")
			benchmark.ReportMetric(float64(total.encodingSteps)/operations, "encode-steps/op")
			benchmark.ReportMetric(float64(totalReadBytes)/operations, "decode-read-B/op")
			benchmark.ReportMetric(0, "encode-read-B/op")
			benchmark.ReportMetric(float64(totalLoads)/operations, "part-loads/op")
			benchmark.ReportMetric(float64(totalEOF)/operations, "eof-probes/op")
			benchmark.ReportMetric(float64(total.decodeAllocatedBytes)/operations, "decode-alloc-B/op")
			benchmark.ReportMetric(float64(total.encodingAllocatedBytes)/operations, "encode-alloc-B/op")
			benchmark.ReportMetric(float64(total.decodeAllocations)/operations, "decode-allocs/op")
			benchmark.ReportMetric(float64(total.encodingAllocations)/operations, "encode-allocs/op")
			benchmark.ReportMetric(float64(total.maxDecoderRetained), "max-decoder-retained-B")
			benchmark.ReportMetric(float64(total.maxEncodingRetained), "max-encoder-retained-B")
			benchmark.ReportMetric(float64(total.maxLiveRetained), "max-live-retained-B")
			benchmark.ReportMetric(float64(total.maxStepLoads), "max-part-loads/step")
			benchmark.ReportMetric(float64(total.maxStepReadBytes), "max-read-B/step")
			benchmark.ReportMetric(float64(total.maxChunkBytes), "max-canonical-chunk-B")
		})
	}
}
