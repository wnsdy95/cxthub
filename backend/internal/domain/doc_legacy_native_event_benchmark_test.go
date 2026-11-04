package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkLegacyNativeEventFrontEnd(benchmark *testing.B) {
	long := strings.Repeat("x", 5*ConversationManifestChunkBytes+73)
	turn := `{"kind":"turn","seq":1,"role":"user"}`
	batch := strings.TrimSuffix(strings.Repeat(turn+",", 512), ",")
	fixtures := []struct {
		name   string
		events string
	}{
		{name: "turn", events: turn},
		{name: "turn-batch512", events: batch},
		{name: "ignored2.5MiB", events: `{"kind":"turn","seq":1,"role":"user","ignored":"` + long + `"}`},
		{name: "selected2.5MiB", events: `{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"` + long + `"}]}`},
	}
	ctx := context.Background()
	for _, fixture := range fixtures {
		raw := []byte(fmt.Sprintf(`{"envelope":{"cir_version":"2"},"events":[%s]}`, fixture.events))
		var original CIRDocument
		if err := json.Unmarshal(raw, &original); err != nil {
			benchmark.Fatal(err)
		}
		header, err := canonicalJSON(original.Envelope)
		if err != nil {
			benchmark.Fatal(err)
		}
		buffer := &legacySourceProofBuffer{body: raw}
		source := buffer.input()
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(raw)))
			totalSteps, totalReads, largest := 0, 0, 0
			for operation := 0; operation < benchmark.N; operation++ {
				buffer.reads = nil
				decoder, err := NewLegacyNormalizationDecoder(ctx, source)
				if err != nil {
					benchmark.Fatal(err)
				}
				seen, complete := 0, false
				for quantum := 0; quantum < 16384; quantum++ {
					before := len(buffer.reads)
					step, err := decoder.Step(ctx)
					if err != nil {
						benchmark.Fatal(err)
					}
					totalSteps++
					loads := 0
					for _, read := range buffer.reads[before:] {
						if read[0] < len(raw) {
							loads++
						}
					}
					largest = max(largest, loads)
					totalReads += loads
					if step.Reset {
						seen = 0
					}
					seen += len(step.Events)
					for _, event := range step.Events {
						if event.Encoding == nil {
							benchmark.Fatal("native front end omitted its actual encoding handoff")
						}
					}
					if step.Complete {
						if seen != len(original.Events) || step.Count != seen || !bytes.Equal(step.Envelope, header) {
							benchmark.Fatal("native front end changed count or original envelope")
						}
						complete = true
						break
					}
				}
				if !complete {
					benchmark.Fatal("native front end did not terminate")
				}
			}
			benchmark.ReportMetric(float64(totalSteps)/float64(benchmark.N), "steps/op")
			benchmark.ReportMetric(float64(totalReads)/float64(benchmark.N), "part-loads/op")
			benchmark.ReportMetric(float64(largest), "max-part-loads/step")
		})
	}
}
