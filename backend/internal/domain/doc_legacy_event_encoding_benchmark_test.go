package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

func BenchmarkLegacyEventEncoding(benchmark *testing.B) {
	prefix := strings.Repeat("p", 16000)
	fixtures := []struct {
		name  string
		event CIREvent
	}{
		{name: "turn", event: CIREvent{Kind: EventTurn, Role: RoleUser}},
		{name: "ascii-2MiB", event: CIREvent{Kind: EventToolResult, Output: strings.Repeat("payload ", 1<<18)}},
		{name: "escaped-unicode", event: CIREvent{Kind: EventToolResult, Output: strings.Repeat("<&>\n\ud55c🙂\u2028", 1<<15)}},
		{name: "wide-structured", event: CIREvent{Kind: EventToolResult, Output: legacyKeyOrderNestedFixture(257)}},
		{name: "long-prefix", event: CIREvent{Kind: EventToolResult, Output: map[string]any{prefix + "z": true, prefix + "a": false, prefix + "m": nil}}},
	}
	ctx := context.Background()
	for _, fixture := range fixtures {
		raw, err := json.Marshal(fixture.event)
		if err != nil {
			benchmark.Fatal(err)
		}
		var event CIREvent
		if err := json.Unmarshal(raw, &event); err != nil {
			benchmark.Fatal(err)
		}
		canonical, err := canonicalInputEventJSON(event)
		if err != nil {
			benchmark.Fatal(err)
		}
		expected := sha256.Sum256(canonical)
		for _, streamed := range []bool{false, true} {
			mode := "materialized"
			if streamed {
				mode = "streamed"
			}
			benchmark.Run(fixture.name+"/"+mode, func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				benchmark.SetBytes(int64(len(canonical)))
				for operation := 0; operation < benchmark.N; operation++ {
					digest := sha256.New()
					length := 0
					if streamed {
						value, err := canonicalEventJSONObject(event)
						if err != nil {
							benchmark.Fatal(err)
						}
						encoding, err := newLegacyCanonicalEncoding(ctx, value)
						if err != nil {
							benchmark.Fatal(err)
						}
						for {
							chunk, complete, err := encoding.Next(ctx)
							if err != nil {
								benchmark.Fatal(err)
							}
							digest.Write(chunk)
							length += len(chunk)
							if complete {
								break
							}
						}
					} else {
						body, err := canonicalInputEventJSON(event)
						if err != nil {
							benchmark.Fatal(err)
						}
						digest.Write(body)
						length = len(body)
					}
					if length != len(canonical) || !bytes.Equal(digest.Sum(nil), expected[:]) {
						benchmark.Fatal("encoding changed original canonical bytes")
					}
				}
			})
		}
	}
}
