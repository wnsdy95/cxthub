package domain

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

var legacyNativeAnyConsumerBenchmarkSink []byte

func BenchmarkLegacyNativeAnyConsumer(benchmark *testing.B) {
	for _, fixture := range []struct {
		name     string
		keyCount int
		putCount int
	}{
		{name: "tiny8keys", keyCount: 8, putCount: 8},
		{name: "dense4096keys", keyCount: 4096, putCount: 4096},
		{name: "duplicate64keys4096puts", keyCount: 64, putCount: 4096},
	} {
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			var event strings.Builder
			event.WriteString(`{"kind":"tool_call","seq":7,"call_id":"consumer","input":{`)
			for putIndex := 0; putIndex < fixture.putCount; putIndex++ {
				if putIndex != 0 {
					event.WriteByte(',')
				}
				keyIndex := putIndex % fixture.keyCount
				fmt.Fprintf(&event, `"key-%04d":`, keyIndex)
				if fixture.putCount > fixture.keyCount && putIndex >= fixture.putCount-fixture.keyCount {
					event.WriteString("null")
					continue
				}
				switch (putIndex/fixture.keyCount + keyIndex) % 3 {
				case 0:
					fmt.Fprintf(&event, "%d.25", putIndex)
				case 1:
					fmt.Fprintf(&event, `"value-%08d"`, putIndex)
				default:
					event.WriteString("null")
				}
			}
			event.WriteString(`}}`)
			raw := legacyNativeEventDocument("2", event.String())
			want, err := legacyNormalizationOracle(raw)
			if err != nil {
				benchmark.Fatal("consumer fixture lost typed canonical acceptance", err)
			}
			buffer := &legacySourceProofBuffer{body: raw}
			source := buffer.input()
			stepLimit := 64 + 32*len(source.Parts)
			buffer.reads = make([][2]int, 0, 2*stepLimit+4)
			readEnds := make([]int, stepLimit+1)
			ctx := context.Background()
			var normalizer *LegacyNormalizationDecoder
			var completed LegacyNormalizationStep
			var bodies [][]byte
			var actual []byte
			var steps, sequence, minimumAdvance, maximumAdvance, maximumEvents, encodingSourceReads int
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(raw)))
			benchmark.ResetTimer()
			for operation := 0; operation < benchmark.N; operation++ {
				buffer.reads = buffer.reads[:0]
				normalizer, err = NewLegacyNormalizationDecoder(ctx, source)
				if err != nil {
					benchmark.Fatal("consumer constructor failed", err)
				}
				readEnds[0] = len(buffer.reads)
				completed, bodies, sequence = LegacyNormalizationStep{}, nil, 0
				for steps = 0; steps < stepLimit; steps++ {
					position := normalizer.position
					step, err := normalizer.Step(ctx)
					if err != nil {
						benchmark.Fatal("consumer normalization step failed", err)
					}
					readEnds[steps+1] = len(buffer.reads)
					advance := normalizer.position - position
					minimumAdvance, maximumAdvance = min(minimumAdvance, advance), max(maximumAdvance, advance)
					maximumEvents = max(maximumEvents, len(step.Events))
					if step.Reset {
						bodies, sequence = nil, 0
					}
					for _, emitted := range step.Events {
						before := len(buffer.reads)
						body, err := legacyNormalizationTestBytes(ctx, emitted)
						if err != nil {
							benchmark.Fatal("consumer canonical encoding failed", err)
						}
						encodingSourceReads += len(buffer.reads) - before
						bodies = append(bodies, body)
						sequence = emitted.Sequence
					}
					if step.Complete {
						completed = step
						break
					}
				}
				if !completed.Complete {
					benchmark.Fatal("consumer exhausted its finite per-part step allowance")
				}
				actual = AssembleCanonicalDoc(completed.Envelope, bodies)
			}
			benchmark.StopTimer()
			if completed.Count != 1 || len(bodies) != 1 || sequence != 7 || !bytes.Equal(actual, want) {
				benchmark.Fatal("consumer changed event count, sequence, or typed canonical bytes")
			}
			if readEnds[0] != 0 || minimumAdvance < 0 || maximumAdvance > ConversationManifestChunkBytes ||
				maximumEvents > LegacyNormalizationStepEvents || encodingSourceReads != 0 {
				benchmark.Fatal("consumer changed lazy reads, step bounds, or owned encoding source independence")
			}
			eofZero, eofOne := 0, 0
			for stepIndex := 0; stepIndex <= steps; stepIndex++ {
				loads := 0
				for _, read := range buffer.reads[readEnds[stepIndex]:readEnds[stepIndex+1]] {
					if read[0] == len(raw) && read[1] == 0 {
						eofZero++
						continue
					}
					if read[0] == len(raw) && read[1] == 1 {
						eofOne++
						continue
					}
					if read[0] < 0 || read[0] >= len(raw) || read[0]%ConversationManifestChunkBytes != 0 ||
						read[1] != min(ConversationManifestChunkBytes, len(raw)-read[0]) {
						benchmark.Fatal("consumer bypassed full authenticated source-part reads", read)
					}
					loads++
				}
				if loads > 2 {
					benchmark.Fatal("consumer exceeded two source-part loads in one step", loads)
				}
			}
			if eofZero != 2 || eofOne != 2 {
				benchmark.Fatal("consumer lost normalization or final-envelope EOF fences", eofZero, eofOne)
			}
			before := len(buffer.reads)
			repeated, err := normalizer.Step(ctx)
			if err != nil || !repeated.Complete || repeated.Reset || len(repeated.Events) != 0 ||
				repeated.Count != completed.Count || !bytes.Equal(repeated.Envelope, completed.Envelope) || len(buffer.reads) != before {
				benchmark.Fatal("completed consumer changed its result or reread the source", err)
			}
			legacyNativeAnyConsumerBenchmarkSink = actual
		})
	}
}
