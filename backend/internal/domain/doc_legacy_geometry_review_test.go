package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyGeometryReviewOversizedIntegerFallbackTerminatesWithinQuantum(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		bytes  int
		field  string
		shape  string
		target reflect.Type
		event  bool
	}{
		{name: "three-leaf-pending", bytes: 2*(64<<10) + 17},
		{name: "nine-leaf-unbounded", bytes: ConversationManifestChunkBytes + 17},
		{name: "large-array-for-string", bytes: 2*(64<<10) + 17, field: "source_model", shape: "array", target: reflect.TypeFor[string]()},
		{name: "large-string-for-bool", bytes: ConversationManifestChunkBytes + 17, field: "agent_message", shape: "string", target: reflect.TypeFor[bool](), event: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			token := "1" + strings.Repeat("0", fixture.bytes-1)
			field, target := fixture.field, fixture.target
			if field == "" {
				field, target = "context_tokens", reflect.TypeFor[int]()
			}
			if fixture.shape == "array" {
				token = `[` + strings.Repeat(`0,`, fixture.bytes/2) + `0]`
			} else if fixture.shape == "string" {
				token = `"` + strings.Repeat("x", fixture.bytes) + `"`
			}
			raw := []byte(`{"envelope":{"cir_version":"2","` + field + `":` + token + `},"events":[]}`)
			if fixture.event {
				raw = []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","role":"user","` + field + `":` + token + `}]}`)
			}
			var original CIRDocument
			var typeError *json.UnmarshalTypeError
			if err := json.Unmarshal(raw, &original); !errors.As(err, &typeError) || typeError.Type != target {
				test.Fatalf("fixture lost its original integer rejection: error type=%T", err)
			}
			for _, geometry := range []struct {
				name  string
				bytes int
			}{
				{name: "original-512k", bytes: ConversationManifestChunkBytes},
				{name: "native-64k", bytes: 64 << 10},
			} {
				test.Run(geometry.name, func(test *testing.T) {
					buffer := &legacySourceProofBuffer{body: raw}
					source := LegacyByteSource{Bytes: len(raw), PartBytes: geometry.bytes, Read: buffer.Read}
					for offset := 0; offset < len(raw); offset += geometry.bytes {
						part := raw[offset:min(offset+geometry.bytes, len(raw))]
						source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
					}
					normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
					if err != nil {
						test.Fatal("valid source geometry was rejected", err)
					}
					var probes [2]int
					limit := 64 + 32*len(source.Parts)
					for stepIndex := 0; stepIndex < limit; stepIndex++ {
						before := len(buffer.reads)
						stepCtx, cancel := context.WithCancel(context.Background())
						step, err := normalizer.Step(stepCtx)
						cancel()
						loads := 0
						for _, read := range buffer.reads[before:] {
							if read[0] == len(raw) && (read[1] == 0 || read[1] == 1) {
								probes[read[1]]++
								continue
							}
							if read[0] < 0 || read[0] >= len(raw) || read[0]%geometry.bytes != 0 ||
								read[1] != min(geometry.bytes, len(raw)-read[0]) {
								test.Fatal("integer fallback bypassed authenticated source-part geometry", read)
							}
							loads++
						}
						if loads > 2 {
							test.Fatalf("integer fallback exceeded the two-load quantum: step=%d loads=%d", stepIndex, loads)
						}
						if step.Complete || step.Reset && !fixture.event || len(step.Events) != 0 || len(step.Envelope) != 0 || step.Count != 0 {
							test.Fatal("rejected envelope emitted a normalization result", stepIndex)
						}
						if err != nil {
							if !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) || probes[0] == 0 || probes[1] == 0 {
								test.Fatal("integer rejection lost authenticated EOF or became a source failure", err, probes)
							}
							return
						}
					}
					test.Fatalf("integer fallback stayed pending past %d source-bounded steps: phase=%d position=%d", limit, normalizer.phase, normalizer.position)
				})
			}
		})
	}
}
