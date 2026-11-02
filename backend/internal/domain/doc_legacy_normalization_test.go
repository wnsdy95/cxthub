package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

func legacyNormalizationTestBytes(ctx context.Context, event LegacyNormalizedEvent) ([]byte, error) {
	if event.Encoding == nil {
		return nil, fmt.Errorf("normalization event has no canonical encoding")
	}
	var body []byte
	for chunkIndex := 0; chunkIndex < 1<<20; chunkIndex++ {
		chunk, done, err := event.Encoding.Next(ctx)
		if err != nil {
			return nil, err
		}
		if len(chunk) > 64<<10 {
			return nil, fmt.Errorf("normalization encoding chunk exceeds 64 KiB: %d", len(chunk))
		}
		body = append(body, chunk...)
		if done {
			return body, nil
		}
	}
	return nil, fmt.Errorf("normalization encoding made no bounded progress")
}

func normalizeLegacyFixture(ctx context.Context, source LegacyByteSource) ([]byte, error) {
	normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
	if err != nil {
		return nil, err
	}
	type materializedEvent struct {
		Sequence int
		Bytes    []byte
	}
	var events []materializedEvent
	for stepIndex := 0; stepIndex < source.Bytes+16; stepIndex++ {
		step, err := normalizer.Step(ctx)
		if err != nil {
			return nil, err
		}
		if step.Reset {
			events = nil
		}
		for _, event := range step.Events {
			preparing := event.Encoding != nil && event.Encoding.preparation != nil
			body, err := legacyNormalizationTestBytes(ctx, event)
			if err != nil {
				if preparing && ctx.Err() == nil {
					normalizer.invalid = true
					continue
				}
				return nil, err
			}
			events = append(events, materializedEvent{Sequence: event.Sequence, Bytes: body})
		}
		if !step.Complete {
			continue
		}
		if len(events) != step.Count {
			return nil, ErrIntegrity
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].Sequence < events[right].Sequence })
		parts := make([][]byte, len(events))
		for ordinal, event := range events {
			parts[ordinal] = event.Bytes
		}
		canonical := AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(parts, []byte(","))})
		if !json.Valid(canonical) {
			return nil, ErrIntegrity
		}
		return canonical, nil
	}
	return nil, fmt.Errorf("normalization cursor made no bounded progress")
}

func legacyNormalizationOracle(raw []byte) ([]byte, error) {
	var document CIRDocument
	if json.Unmarshal(raw, &document) != nil {
		return nil, ErrIntegrity
	}
	canonical, err := CanonicalBytes(document)
	if err != nil || !json.Valid(canonical) {
		return nil, ErrIntegrity
	}
	return canonical, nil
}

func TestLegacyNormalizationDecoderTypedParity(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	fixtures := []string{
		`null`, `{}`, `{"events":null}`, `{"envelope":null,"events":[]}`,
		`{"ignored":1e9999,"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":0,"role":"user"}]}`,
		`{"EnVeLoPe":{"CIR_VERSION":"2"},"event\u017F":[{"kind":"turn","seq":1,"role":"system"}]}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":false}],"envelope":{"cir_version":"2"}}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"unknown"}],"events":[{"kind":"turn","role":"user"}]}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_author":"invalid"}],"events":null}`,
		`{"envelope":{"cir_version":"2","source_models":["first","second"]},"envelope":{"source_models":["changed"]},"envelope":{"source_models":[null,null]}}`,
		`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","blocks":[{"type":"text","text":"old"},{"type":"text","text":"tail"}],"blocks":[{"text":"new"}],"blocks":[null,{}]}]}`,
		`{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_call","input":{"keep":1,"replace":{"old":1}},"input":{"replace":{"new":2}}},{"kind":"tool_result","output":{"old":1},"output":{"new":2}}]}`,
		`{"events":[{"kind":"turn","seq":"bad"}],"events":[]}`,
		`{"envelope":{"cir_version":7},"envelope":{"cir_version":"2"}}`,
		`{"events":[{"kind":"turn"},{"kind":"turn","seq":"bad"}]}`,
		`{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":false}]}`,
		`{"envelope":{"cir_version":"99"}}`,
		`{"events":[null]}`, `{"events":{}}`, `{"envelope":[]}`, `[]`, `true`, `1`,
		`{"events":[],}`, `{"events":[,]}`, `{"events":[{} {}]}`, `{"events":[]`,
		`{}null`, `null[]`, `{"events":[]} garbage`, `{"ignored":[}`, ``,
		fmt.Sprintf(`{"events":[{"kind":"turn","id":"max","seq":%d},{"kind":"turn","id":"min-first","seq":%d},{"kind":"turn","id":"min-second","seq":%d}]}`, maximum, -maximum-1, -maximum-1),
		"{\"envelope\":{\"source_model\":\"\xff\"},\"ignored\":\"\xed\xa0\x80\"}",
	}
	for ordinal, raw := range fixtures {
		test.Run(fmt.Sprint(ordinal), func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(raw)}
			got, err := normalizeLegacyFixture(context.Background(), buffer.input())
			want, wantErr := legacyNormalizationOracle([]byte(raw))
			if !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
				test.Fatalf("typed normalization changed: got=%q error=%v want=%q error=%v", got, err, want, wantErr)
			}
		})
	}
}

func TestLegacyNormalizationDecoderRootDepthBudget(test *testing.T) {
	for _, depth := range []int{9998, 9999, 10000} {
		test.Run(fmt.Sprint(depth), func(test *testing.T) {
			raw := []byte(`{"ignored":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`)
			buffer := &legacySourceProofBuffer{body: raw}
			got, err := normalizeLegacyFixture(context.Background(), buffer.input())
			want, wantErr := legacyNormalizationOracle(raw)
			if !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
				test.Fatal("root nesting budget changed", depth, err, wantErr)
			}
		})
	}
}

func TestLegacyNormalizationDecoderBatchesDoNotRetainEvents(test *testing.T) {
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"2"},"events":[`)
	const count = 3*LegacyNormalizationStepEvents + 1
	for ordinal := range count {
		if ordinal > 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"kind":"message","seq":%d,"role":"user","blocks":[{"type":"text","text":"%s"}]}`, count-ordinal, strings.Repeat("payload", 700))
	}
	raw.WriteString(`]}`)
	buffer := &legacySourceProofBuffer{body: []byte(raw.String())}
	source := buffer.input()
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	source.Parts[0].Hash = HashContent([]byte("caller changed descriptor"))
	seen, batches := 0, 0
	for stepIndex := 0; stepIndex < 100; stepIndex++ {
		step, err := normalizer.Step(context.Background())
		if err != nil || len(step.Events) > LegacyNormalizationStepEvents {
			test.Fatal("decoder changed its own source or exceeded event batch", err)
		}
		if len(step.Events) > 0 {
			batches++
			seen += len(step.Events)
		}
		retained, err := normalizer.RetainedBytes()
		if err != nil || retained >= 64<<10 {
			test.Fatal("decoder retained decoded event batch or whole root", retained, err)
		}
		if step.Complete {
			if seen != count || step.Count != count || batches < 4 {
				test.Fatal("missing selected events or missing bounded yields", seen, step.Count, batches)
			}
			original := bytes.Clone(step.Envelope)
			step.Envelope[0] ^= 1
			repeated, err := normalizer.Step(context.Background())
			if err != nil || !repeated.Complete || !bytes.Equal(repeated.Envelope, original) {
				test.Fatal("completed header aliases caller-owned output", err)
			}
			return
		}
	}
	test.Fatal("decoder did not complete")
}

func TestLegacyNormalizationDecoderPrivateFailureOutranksRejectedDocument(test *testing.T) {
	raw := []byte(`{"envelope":42,"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `"}`)
	buffer := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	buffer.body[len(buffer.body)-2] ^= 1
	for stepIndex := 0; stepIndex < 20; stepIndex++ {
		step, err := normalizer.Step(context.Background())
		if step.Complete || len(step.Events) != 0 {
			test.Fatal("rejected source exposed partial normalization")
		}
		if err != nil {
			if !errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("permanent rejection hid private-source corruption", err)
			}
			return
		}
	}
	test.Fatal("decoder did not finish source authentication")
}

func TestLegacyNormalizationDecoderCancellationAndEOF(test *testing.T) {
	for _, failureAtEOF := range []bool{false, true} {
		test.Run(fmt.Sprint(failureAtEOF), func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(`{"events":[]}`)}
			source := buffer.input()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if offset == len(buffer.body) && length == 1 {
					if failureAtEOF {
						return nil, ErrPreparedDocRangeObject
					}
					cancel()
					return nil, ErrIntegrity
				}
				return buffer.Read(ctx, offset, length)
			}
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			if err != nil {
				test.Fatal(err)
			}
			for stepIndex := 0; stepIndex < 20; stepIndex++ {
				step, err := normalizer.Step(ctx)
				if step.Complete {
					test.Fatal("EOF failure created a completed result")
				}
				if err != nil {
					want := error(context.Canceled)
					if failureAtEOF {
						want = ErrPreparedDocRangeObject
					}
					if !errors.Is(err, want) || len(step.Events) != 0 || step.Envelope != nil || step.Reset {
						test.Fatal("EOF/cancellation precedence or atomicity changed", err)
					}
					return
				}
			}
			test.Fatal("EOF failure was swallowed")
		})
	}
}

func TestLegacyNormalizationDecoderCountsHiddenEnvelopeBacking(test *testing.T) {
	tail := strings.Repeat("hidden-model", 6000)
	raw := []byte(fmt.Sprintf(`{"envelope":{"source_models":["first",%q]},"envelope":{"source_models":["changed"]},"padding":%q,"envelope":{"source_models":[null,null]}}`, tail, strings.Repeat("x", ConversationManifestChunkBytes)))
	buffer := &legacySourceProofBuffer{body: raw}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	hiddenChecked := false
	for stepIndex := 0; stepIndex < 20; stepIndex++ {
		step, err := normalizer.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if len(normalizer.envelope.SourceModels) == 1 {
			retained, err := normalizer.RetainedBytes()
			if err != nil || retained < int64(len(tail)) || cap(normalizer.envelope.SourceModels) < 2 {
				test.Fatal("shortened source_models hid a retained backing string", retained, err)
			}
			hiddenChecked = true
		}
		if step.Complete {
			var envelope CIREnvelope
			if json.Unmarshal(step.Envelope, &envelope) != nil || !hiddenChecked || len(envelope.SourceModels) != 2 || envelope.SourceModels[0] != "changed" || envelope.SourceModels[1] != tail {
				test.Fatal("envelope reduction discarded hidden string-slice history")
			}
			return
		}
	}
	test.Fatal("envelope normalization did not finish")
}

func TestLegacyNormalizationDecoderJoinedPrivateEOFDoesNotComplete(test *testing.T) {
	buffer := &legacySourceProofBuffer{body: []byte(`{}` + strings.Repeat(" ", 2*ConversationManifestChunkBytes))}
	source := buffer.input()
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if offset >= ConversationManifestChunkBytes && offset < len(buffer.body) {
			return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
		}
		return buffer.Read(ctx, offset, length)
	}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	for stepIndex := 0; stepIndex < 10; stepIndex++ {
		step, err := normalizer.Step(context.Background())
		if step.Complete || step.Envelope != nil {
			test.Fatal("private EOF was treated as successful document EOF")
		}
		if err != nil {
			if !errors.Is(err, ErrPreparedDocRangeObject) || normalizer.phase == legacyNormalizationComplete {
				test.Fatal("private EOF sealed or permanently rejected a provisional result", err)
			}
			repeated, repeatedErr := normalizer.Step(context.Background())
			if repeated.Complete || repeated.Envelope != nil || !errors.Is(repeatedErr, ErrPreparedDocRangeObject) {
				test.Fatal("failed source became complete on a repeated call", repeatedErr)
			}
			return
		}
	}
	test.Fatal("private EOF was never observed")
}

func TestLegacyNormalizationDecoderRequiresOriginalProofProducer(test *testing.T) {
	raw := []byte(`{"events":[{"kind":"turn","seq":9,"role":"user"},{"kind":"turn","seq":-2,"role":"system"}],"envelope":{"cir_version":"1"}}`)
	input := &legacySourceProofBuffer{body: raw}
	canonical, err := normalizeLegacyFixture(context.Background(), input.input())
	if err != nil || bytes.Equal(canonical, raw) {
		test.Fatal("fixture did not need normalization", err)
	}
	for _, claimed := range []ContentHash{HashContent(canonical), HashContent(raw)} {
		test.Run(string(claimed), func(test *testing.T) {
			composed := &legacySourceProofBuffer{body: bytes.Clone(canonical)}
			target := &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			producer, err := NewLegacyByteSourcePreparation(context.Background(), claimed, composed.input(), target, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			progress, err := legacySourceProofDrain(test, producer)
			if err != nil {
				test.Fatal(err)
			}
			proof, proofErr := producer.Result()
			if claimed != HashContent(canonical) {
				if progress != LegacyByteSourceOriginalRequired || proof.Valid() || !errors.Is(proofErr, ErrDocRangeIndexPending) {
					test.Fatal("provisional parser output authorized the wrong claimed source", progress, proofErr)
				}
				return
			}
			if progress != LegacyByteSourceVerified || proofErr != nil || !proof.Valid() {
				test.Fatal("original producer could not seal the composed canonical source", progress, proofErr)
			}
		})
	}
}

func TestLegacyNormalizationDecoderEOFProbeIOFailureIsRetryable(test *testing.T) {
	for _, raw := range []string{`{"events":[]}`, `{"envelope":42}`} {
		test.Run(raw, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(raw)}
			source := buffer.input()
			probeFailure := errors.New("transient private-file EOF probe failure")
			failed := false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if offset == len(buffer.body) && length == 1 && !failed {
					failed = true
					return nil, probeFailure
				}
				return buffer.Read(ctx, offset, length)
			}
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
			if err != nil {
				test.Fatal(err)
			}
			for stepIndex := 0; stepIndex < 20; stepIndex++ {
				step, err := normalizer.Step(context.Background())
				if step.Complete {
					test.Fatal("IO failure completed provisional normalization")
				}
				if err != nil {
					if !errors.Is(err, probeFailure) || !errors.Is(err, ErrPreparedDocRangeObject) || len(step.Events) != 0 || step.Envelope != nil || step.Reset {
						test.Fatal("private IO error was lost or turned into permanent document rejection", err)
					}
					return
				}
			}
			test.Fatal("IO failure was swallowed")
		})
	}
}

func TestLegacyNormalizationDecoderTinyRootMembersHaveBoundedReads(test *testing.T) {
	raw := []byte(`{` + strings.Repeat(`"x":0,`, 4096) + `"events":[]}`)
	buffer := &legacySourceProofBuffer{body: raw}
	got, err := normalizeLegacyFixture(context.Background(), buffer.input())
	want, wantErr := legacyNormalizationOracle(raw)
	if err != nil || wantErr != nil || !bytes.Equal(got, want) {
		test.Fatal("tiny ignored members changed normalization", err, wantErr)
	}
	readBytes := 0
	for _, read := range buffer.reads {
		readBytes += read[1]
	}
	if readBytes > 8*len(raw) {
		test.Fatalf("tiny root members amplified physical part reads: input=%d read=%d calls=%d maximum=%d", len(raw), readBytes, len(buffer.reads), 8*len(raw))
	}
	test.Logf("input=%d physical_read_bytes=%d calls=%d", len(raw), readBytes, len(buffer.reads))
}

func TestLegacyNormalizationDecoderTinyRootMembersCrossPartWindow(test *testing.T) {
	raw := []byte(`{` + strings.Repeat(`"x":0,`, 90000) + `"events":null,"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":-1,"role":"system"}]}`)
	buffer := &legacySourceProofBuffer{body: raw}
	got, err := normalizeLegacyFixture(context.Background(), buffer.input())
	want, wantErr := legacyNormalizationOracle(raw)
	if err != nil || wantErr != nil || !bytes.Equal(got, want) {
		test.Fatal("root-member batch lost its parser context or selection across a part boundary", err, wantErr)
	}
	readBytes := 0
	for _, read := range buffer.reads {
		readBytes += read[1]
	}
	if readBytes > 8*len(raw) {
		test.Fatalf("cross-part root members amplified physical reads: input=%d read=%d calls=%d maximum=%d", len(raw), readBytes, len(buffer.reads), 8*len(raw))
	}
	test.Logf("input=%d physical_read_bytes=%d calls=%d", len(raw), readBytes, len(buffer.reads))
}

func FuzzLegacyNormalizationDecoderParity(fuzzer *testing.F) {
	for _, seed := range []string{`null`, `{}`, `{"events":[{"kind":"turn","seq":1}]}`, `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":{"keep":1},"output":null}]}`} {
		fuzzer.Add([]byte(seed))
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 4096 {
			return
		}
		buffer := &legacySourceProofBuffer{body: raw}
		got, err := normalizeLegacyFixture(context.Background(), buffer.input())
		want, wantErr := legacyNormalizationOracle(raw)
		if !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
			test.Fatalf("whole typed oracle differs: raw=%q got=%q error=%v want=%q error=%v", raw, got, err, want, wantErr)
		}
	})
}
