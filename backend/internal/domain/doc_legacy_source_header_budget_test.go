package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacySourceHeaderBudgetEnvelope(name string) CIREnvelope {
	envelope := CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, Fidelity: FidelityFull}
	switch name {
	case "string":
		envelope.Cwd = strings.Repeat("s", 2*ConversationManifestChunkBytes+73)
	case "list":
		envelope.Cwd = strings.Repeat("l", ConversationManifestChunkBytes+17)
		envelope.SourceModels = make([]string, 4*legacyCanonicalEncodingActions+1)
		for position := range envelope.SourceModels {
			envelope.SourceModels[position] = fmt.Sprintf("model-%d", position%17)
		}
	case "escaping":
		envelope.SourceModel = strings.Repeat("한🙂<&>\u2028\u2029\"\\\x00\n", 32769)
	}
	return envelope
}

func legacySourceHeaderBudgetDocument(test *testing.T, envelope CIREnvelope) ([]byte, []byte) {
	test.Helper()
	header, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := CanonicalBytes(CIRDocument{Envelope: envelope, Events: []CIREvent{{Kind: EventTurn, Seq: 0}}})
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(raw[len(canonicalDocPrefix):len(canonicalDocPrefix)+len(header)], header) {
		test.Fatal("fixture header differs from the original exact-byte canonical oracle")
	}
	return raw, header
}

func legacySourceHeaderBudgetReads(test *testing.T, reads [][2]int, extent int) (int, int) {
	test.Helper()
	payload, probes := 0, 0
	for _, read := range reads {
		if read[0] == extent {
			if read[1] != 0 && read[1] != 1 {
				test.Fatal("source EOF probe changed geometry", read)
			}
			probes++
			continue
		}
		if read[0] < 0 || read[0] >= extent || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, extent-read[0]) {
			test.Fatal("source header bypassed original authenticated 512 KiB parts", read, extent)
		}
		payload++
	}
	if payload > 2 || probes > 2 || payload != 0 && probes != 0 {
		test.Fatal("source header combined payload loads and EOF probes or exceeded two loads", payload, probes)
	}
	if probes != 0 && (probes != 2 || reads[0] != [2]int{extent, 0} || reads[1] != [2]int{extent, 1}) {
		test.Fatal("source header did not preserve the exact two-probe EOF quantum", reads)
	}
	return payload, probes
}

func legacySourceHeaderBudgetHashBytes(test *testing.T, state []byte, maximum int) int {
	test.Helper()
	if len(state) == 0 {
		return 0
	}
	if len(state) < 8 {
		test.Fatal("actual envelope SHA state lost its byte count", len(state))
	}
	count := binary.BigEndian.Uint64(state[len(state)-8:])
	if count > uint64(maximum) {
		test.Fatal("actual envelope SHA state exceeds the exact-byte header", count, maximum)
	}
	return int(count)
}

func TestLegacySourceHeaderBudgetActualPreparation(test *testing.T) {
	if ConversationManifestChunkBytes != 512<<10 || LegacyCanonicalEncodingBytes != 64<<10 || legacyCanonicalEncodingActions != 1024 {
		test.Fatal("source header changed the original source or encoding guards")
	}
	for _, name := range []string{"string", "list", "escaping"} {
		test.Run(name, func(test *testing.T) {
			raw, header := legacySourceHeaderBudgetDocument(test, legacySourceHeaderBudgetEnvelope(name))
			if len(header) <= ConversationManifestChunkBytes {
				test.Fatal("supported header fixture no longer crosses the original source quantum", len(header))
			}
			if _, err := decodeCanonicalCIREnvelope(header); err != nil {
				test.Fatal("original strict exact-byte header oracle rejected the supported fixture", err)
			}
			input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
			source := input.input()
			var current context.Context
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if ctx != current || ctx.Err() != nil {
					test.Fatal("actual preparation reused an earlier or canceled quantum context", offset, length)
				}
				return input.Read(ctx, offset, length)
			}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), source, output, &verifier)
			if err != nil || len(input.reads) != 0 || len(output.body) != 0 {
				test.Fatal("source preparation constructor performed payload work", err)
			}
			headerQuanta, headerPayload, headerEOF, maximumActions, encodedBytes := 0, 0, 0, 0, 0
			output.onAppend = func() {
				if preparation.version != CIRVersionV2 || preparation.envelope != nil || headerEOF < 4 || encodedBytes != len(header) {
					test.Fatal("target received event bytes before strict header approval")
				}
			}
			for quantum := 0; quantum < 4096; quantum++ {
				if proof, err := preparation.Result(); proof.Valid() || !errors.Is(err, ErrDocRangeIndexPending) {
					test.Fatal("pending header or stream produced publication authority", quantum, err)
				}
				beforeReads, beforeWrites, version := len(input.reads), len(output.body), preparation.version
				base, cancel := context.WithCancel(context.Background())
				ctx := &canonicalAdmissionContext{Context: base, check: func() {}}
				helper := preparation.envelope
				actions, operations, beforeHashed := 0, 0, 0
				if helper != nil && helper.encoding != nil {
					encoding := helper.encoding
					state := func() string {
						return fmt.Sprintf("%d/%d/%d/%p/%d/%t", encoding.phase, encoding.field, encoding.model, encoding.scalar, len(encoding.pending), encoding.complete)
					}
					previous := state()
					operation, _ := encoding.stepWork()
					beforeHashed = legacySourceHeaderBudgetHashBytes(test, helper.hashState, len(header))
					ctx.check = func() {
						next := state()
						if next != previous {
							actions++
							operations += operation
							previous = next
							operation, _ = encoding.stepWork()
						}
					}
				}
				current = ctx
				progress, err := preparation.Step(ctx)
				cancel()
				payload, probes := legacySourceHeaderBudgetReads(test, input.reads[beforeReads:], len(raw))
				if actions != 0 {
					afterHashed := legacySourceHeaderBudgetHashBytes(test, helper.hashState, len(header))
					hashed := afterHashed - beforeHashed
					if actions > legacyCanonicalEncodingActions || hashed < 0 || hashed > LegacyCanonicalEncodingBytes || operations+hashed > ConversationManifestChunkBytes || payload != 0 || probes != 0 {
						test.Fatal("actual envelope helper exceeded its shared action/output/work quantum", quantum, actions, hashed, operations, payload, probes)
					}
					wantState, stateErr := legacySourceHashAppend(context.Background(), nil, header[:afterHashed])
					if stateErr != nil || !bytes.Equal(helper.hashState, wantState) {
						test.Fatal("actual envelope hash no longer matches the original exact-byte prefix oracle", quantum, stateErr)
					}
					maximumActions = max(maximumActions, actions)
					encodedBytes = afterHashed
				} else if helper != nil && helper.phase == legacySourceEnvelopeEncoding && helper.encoding != nil && beforeHashed != 0 {
					test.Fatal("fresh actual encoding quantum made no action or byte progress", quantum)
				}
				if version == "" && preparation.position == len(raw) {
					headerQuanta++
					headerPayload += payload
					headerEOF += probes
				}
				if err != nil || !bytes.Equal(input.body, raw) {
					test.Fatal("actual large supported header failed or changed source bytes", quantum, progress, err)
				}
				if preparation.version == "" && len(output.body) != beforeWrites {
					test.Fatal("actual preparation wrote target bytes while strict header remained pending", quantum)
				}
				if progress == LegacyByteSourceVerified {
					proof, err := preparation.Result()
					streamStart := len(canonicalDocPrefix) + len(header) + len(canonicalDocMiddle)
					if err != nil || !proof.Valid() || !bytes.Equal(output.body, raw[streamStart:len(raw)-len(canonicalDocSuffix)]) {
						test.Fatal("incremental header changed the original exact-byte target stream", err)
					}
					if headerQuanta < 4 || headerPayload < 3 || headerEOF < 4 || maximumActions <= 1 || name == "list" && maximumActions != legacyCanonicalEncodingActions {
						test.Fatal("strict header bypassed resumable decode, batched encoding, or EOF", headerQuanta, headerPayload, headerEOF, maximumActions)
					}
					return
				}
				if progress != LegacyByteSourcePending || len(verifier.events) != 0 {
					test.Fatal("pending header escaped or primed semantic cache authority", quantum, progress, len(verifier.events))
				}
			}
			test.Fatal("actual large supported header exhausted its finite progress allowance")
		})
	}
}

func TestLegacySourceHeaderBudgetNativeAndEncoderSteps(test *testing.T) {
	for _, name := range []string{"string", "list", "escaping"} {
		test.Run(name, func(test *testing.T) {
			envelope := legacySourceHeaderBudgetEnvelope(name)
			raw, header := legacySourceHeaderBudgetDocument(test, envelope)
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			source := input.input()
			var current context.Context
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if ctx != current || ctx.Err() != nil {
					test.Fatal("native header decoder reused a previous quantum context")
				}
				return input.Read(ctx, offset, length)
			}
			var decoded CIREnvelope
			native, err := newLegacyNativeObject(reflect.ValueOf(&decoded), &legacyEnvelopeNativeFields, 2, false)
			if err != nil {
				test.Fatal(err)
			}
			native.start(len(canonicalDocPrefix), len(canonicalDocPrefix)+len(header))
			var frame legacyJSONValueScanner
			if err := frame.reset(2); err != nil {
				test.Fatal(err)
			}
			decodeQuanta := 0
			for ; decodeQuanta < 4096; decodeQuanta++ {
				ctx, cancel := context.WithCancel(context.Background())
				current = ctx
				stepInput := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
				beforeReads, beforePosition := len(input.reads), native.progressPosition()
				complete, err := native.step(ctx, stepInput, &frame)
				cancel()
				payload, probes := legacySourceHeaderBudgetReads(test, input.reads[beforeReads:], len(raw))
				if err != nil || stepInput.failure != nil || stepInput.unbounded || stepInput.workBytes < 0 || stepInput.workBytes > ConversationManifestChunkBytes ||
					stepInput.loads != payload || probes != 0 || stepInput.fields < 0 || stepInput.fields > native.fieldActionLimit() ||
					native.progressPosition() < beforePosition || native.progressPosition() > len(canonicalDocPrefix)+len(header) {
					test.Fatal("native header exceeded its actual shared byte/load/field quantum", decodeQuanta, stepInput.workBytes, stepInput.loads, stepInput.fields, err)
				}
				if complete {
					break
				}
				if stepInput.workBytes == 0 && stepInput.fields == 0 {
					test.Fatal("fresh native header quantum made no accounted progress", decodeQuanta)
				}
			}
			if decodeQuanta < 2 || decodeQuanta >= 4096 || !reflect.DeepEqual(decoded, envelope) {
				test.Fatal("native header failed to yield or changed original typed values", decodeQuanta)
			}
			var heap docRangeHeaderHeapEstimate
			for _, field := range native.heaps {
				heap.include(field)
			}
			encoding, err := newLegacyEnvelopeEncoding(context.Background(), decoded, heap)
			if err != nil {
				test.Fatal(err)
			}
			var encoded []byte
			for action := 0; action < 65536; action++ {
				before := fmt.Sprintf("%d/%d/%d/%p/%d", encoding.phase, encoding.field, encoding.model, encoding.scalar, len(encoding.pending))
				scalar, position, sourceLength := encoding.scalar, 0, 0
				if scalar != nil {
					if text, ok := scalar.root.(string); ok {
						sourceLength = len(text)
						if scalar.frames.length() != 0 {
							position = scalar.frames.at(0).position
						}
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				operation, output := encoding.stepWork()
				chunk, complete, err := encoding.Step(ctx)
				cancel()
				if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes || len(chunk) > output || operation+len(chunk) > ConversationManifestChunkBytes {
					test.Fatal("actual envelope encoder exceeded the original output quantum", action, len(chunk), cap(chunk), err)
				}
				if scalar != nil && scalar.frames.length() != 0 && scalar.frames.at(0).position-position > LegacyCanonicalEncodingBytes ||
					scalar != nil && scalar.frames.length() == 0 && sourceLength-position > LegacyCanonicalEncodingBytes {
					test.Fatal("actual scalar encoder exceeded the original input quantum", action, position, sourceLength)
				}
				encoded = append(encoded, chunk...)
				if complete {
					if !bytes.Equal(encoded, header) || !bytes.Equal(input.body, raw) {
						test.Fatal("native decode and actual encoder differ from original exact-byte header oracle")
					}
					return
				}
				after := fmt.Sprintf("%d/%d/%d/%p/%d", encoding.phase, encoding.field, encoding.model, encoding.scalar, len(encoding.pending))
				if len(chunk) == 0 && before == after && (scalar == nil || scalar.frames.length() == 0 || scalar.frames.at(0).position == position) {
					test.Fatal("actual envelope encoder made no structural or scalar progress", action)
				}
			}
			test.Fatal("actual envelope encoder exhausted its finite action allowance")
		})
	}
}

func TestLegacySourceHeaderBudgetLargeIntegerNeverWritesTarget(test *testing.T) {
	envelope := legacySourceHeaderBudgetEnvelope("string")
	envelope.ContextTokens = 1
	raw, header := legacySourceHeaderBudgetDocument(test, envelope)
	badInteger := []byte(`"context_tokens":` + strings.Repeat("9", LegacyCanonicalEncodingBytes+1))
	raw = bytes.Replace(raw, []byte(`"context_tokens":1`), badInteger, 1)
	header = bytes.Replace(header, []byte(`"context_tokens":1`), badInteger, 1)
	if _, err := decodeCanonicalCIREnvelope(header); !errors.Is(err, ErrIntegrity) {
		test.Fatal("large integer fixture no longer exercises original typed rejection", err)
	}
	input, output := &legacySourceProofBuffer{body: bytes.Clone(raw)}, &legacySourceProofBuffer{}
	source := input.input()
	var current context.Context
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if ctx != current || ctx.Err() != nil {
			test.Fatal("large integer path reused an earlier or canceled context")
		}
		return input.Read(ctx, offset, length)
	}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), source, output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	for quantum := 0; quantum < 4096; quantum++ {
		before := len(input.reads)
		ctx, cancel := context.WithCancel(context.Background())
		current = ctx
		progress, err := preparation.Step(ctx)
		cancel()
		legacySourceHeaderBudgetReads(test, input.reads[before:], len(raw))
		proof, resultErr := preparation.Result()
		if len(output.body) != 0 || len(verifier.events) != 0 || proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) {
			test.Fatal("invalid large integer wrote target bytes or minted publication authority", quantum, progress, err, resultErr)
		}
		if err != nil {
			if !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("large typed integer changed the original semantic error class", err)
			}
			return
		}
		if progress != LegacyByteSourcePending {
			test.Fatal("invalid large integer escaped strict header rejection", quantum, progress)
		}
	}
	test.Fatal("invalid large integer exhausted its finite rejection allowance")
}
