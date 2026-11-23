package domain

import (
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
	_ = context.Background()
	for _, fixture := range fixtures {
		raw := []byte(fmt.Sprintf(`{"envelope":{"cir_version":"2"},"events":[%s]}`, fixture.events))
		var original CIRDocument
		if err := json.Unmarshal(raw, &original); err != nil {
			benchmark.Fatal(err)
		}
		_, err := canonicalJSON(original.Envelope)
		if err != nil {
			benchmark.Fatal(err)
		}
		buffer := &legacySourceProofBuffer{body: raw}
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			benchmark.SetBytes(int64(len(raw)))
			totalSteps, totalReads, largest := 0, 0, 0
			for operation := 0; operation < benchmark.N; operation++ {
				buffer.reads = nil
				if err != nil {
					benchmark.Fatal(err)
				}
				_, complete := 0, false
				for quantum := 0; quantum < 16384; quantum++ {
					before := len(buffer.reads)
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
