package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkLegacyNativePagedPublicSingleShot(benchmark *testing.B) {
	benchmarkLegacyNativePublicSingleShot(benchmark, false)
}

func BenchmarkLegacyNativeBlockPagedPublicSingleShot(benchmark *testing.B) {
	benchmarkLegacyNativePublicSingleShot(benchmark, true)
}

func benchmarkLegacyNativePublicSingleShot(benchmark *testing.B, blocks bool) {
	benchmark.StopTimer()
	for name, expected := range map[string]string{"test.benchtime": "1x", "test.count": "1", "test.cpu": "1"} {
		configured := flag.Lookup(name)
		if configured == nil || configured.Value.String() != expected {
			benchmark.Skip("parent-only serial single-shot diagnostic requires -benchtime=1x -count=1 -cpu=1")
		}
	}
	for _, count := range []int{8, 8192} {
		benchmark.Run(fmt.Sprint(count), func(benchmark *testing.B) {
			benchmark.StopTimer()
			if benchmark.N != 1 {
				benchmark.Skip("public normalization diagnostic disables repeated calibration")
			}
			child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
			event := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", count-1) + child + `],"replacement_complete":true}`
			if blocks {
				block := `{"type":"text","text":"` + strings.Repeat("x", 64) + `"}`
				event = `{"kind":"message","role":"assistant","blocks":[` + strings.Repeat(block+",", count-1) + block + `]}`
			}
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + event + `]}`)
			if !blocks && count == 8192 && len(event) != 1138753 {
				benchmark.Fatal("public cost fixture lost the original 8192-child source")
			}
			if blocks && count == 8192 && len(event) <= ConversationManifestChunkBytes {
				benchmark.Fatal("public block cost fixture lost its original multipart source")
			}
			want, err := legacyNormalizationOracle(raw)
			if err != nil {
				benchmark.Fatal(err)
			}
			var expected struct {
				Envelope json.RawMessage   `json:"envelope"`
				Events   []json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(want, &expected); err != nil || len(expected.Events) != 1 {
				benchmark.Fatal("public cost fixture lost its independent typed oracle", err)
			}
			buffer := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			source := LegacyByteSource{Bytes: len(raw), PartBytes: LegacyNormalizationPartBytes, Read: buffer.Read}
			for offset := 0; offset < len(raw); offset += LegacyNormalizationPartBytes {
				part := raw[offset:min(offset+LegacyNormalizationPartBytes, len(raw))]
				source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
			}
			digest := sha256.New()
			digest.Write([]byte(canonicalDocPrefix))
			digest.Write(expected.Envelope)
			digest.Write([]byte(canonicalDocMiddle))
			maximum, decodeSteps, encodeSteps, emitted, outputBytes := int64(0), 0, 0, 0, 0
			observe := func(value interface{ RetainedBytes() (int64, error) }) {
				retained, err := value.RetainedBytes()
				if err != nil || retained <= 0 {
					benchmark.Fatal("public normalization lost retained accounting", retained, err)
				}
				maximum = max(maximum, retained)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			benchmark.StartTimer()
			decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
			benchmark.StopTimer()
			if err != nil {
				benchmark.Fatal(err)
			}
			observe(decoder)
			complete := false
			for quantum := 0; quantum < 8192; quantum++ {
				before := len(buffer.reads)
				benchmark.StartTimer()
				step, err := decoder.Step(context.Background())
				benchmark.StopTimer()
				decodeSteps++
				if err != nil || len(buffer.reads)-before > 2 {
					benchmark.Fatal("public normalization failed or exceeded its source load quantum", err)
				}
				observe(decoder)
				for _, pending := range step.Events {
					emitted++
					encodingComplete := false
					for action := 0; action < 65536; action++ {
						benchmark.StartTimer()
						chunk, done, err := pending.Encoding.Next(context.Background())
						benchmark.StopTimer()
						encodeSteps++
						if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
							benchmark.Fatal("public canonical output failed or exceeded its original quantum", err)
						}
						digest.Write(chunk)
						outputBytes += len(chunk)
						observe(pending.Encoding)
						if done {
							encodingComplete = true
							break
						}
					}
					if !encodingComplete {
						benchmark.Fatal("public canonical output exhausted its finite allowance")
					}
				}
				if step.Complete {
					if step.Count != 1 || emitted != 1 || !bytes.Equal(step.Envelope, expected.Envelope) {
						benchmark.Fatal("public normalization changed its typed count or canonical envelope")
					}
					complete = true
					break
				}
			}
			digest.Write([]byte(canonicalDocSuffix))
			if !complete || ContentHash("sha256:"+hex.EncodeToString(digest.Sum(nil))) != HashContent(want) || !bytes.Equal(raw, buffer.body) {
				benchmark.Fatal("public normalization changed the independent exact-byte oracle SHA or original source")
			}
			var readBytes int64
			loads, probes := 0, 0
			for _, read := range buffer.reads {
				if read[0] == len(raw) {
					probes++
				} else {
					loads++
					readBytes += int64(read[1])
				}
			}
			benchmark.ReportMetric(float64(decodeSteps), "decode-Steps/op")
			benchmark.ReportMetric(float64(encodeSteps), "encode-Next/op")
			benchmark.ReportMetric(float64(loads), "authenticated-loads/op")
			benchmark.ReportMetric(float64(readBytes), "source-read-B/op")
			benchmark.ReportMetric(float64(probes), "EOF-probes/op")
			benchmark.ReportMetric(float64(maximum), "max-observed-component-retained-B")
			benchmark.ReportMetric(float64(outputBytes), "event-output-B/op")
			benchmark.Logf("source-B=%d source-SHA=%x canonical-SHA=%s; timed actual public decoder constructor/Steps and canonical Next including current authenticated source reads/copies/hashes. Fixture, typed oracle, source manifest, output SHA, observation, release and timer transitions untimed; Step-boundary component estimate is not combined peak/RSS/allocator pause, PG lifecycle, production p95, or durable recovery proof.", len(raw), sha256.Sum256(raw), HashContent(want))
		})
	}
}
