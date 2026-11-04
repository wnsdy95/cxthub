package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

var legacyNativeAnyFenceRoutes = []struct {
	name    string
	field   string
	prefix  string
	suffix  string
	parents int
}{
	{name: "input-object", field: "input", prefix: `{"kind":"tool_call","input":{"value":{"payload":`, suffix: `}}}`, parents: 5},
	{name: "input-array", field: "input", prefix: `{"kind":"tool_call","input":{"value":[`, suffix: `]}}`, parents: 5},
	{name: "output-object", field: "output", prefix: `{"kind":"tool_result","output":{"value":{"payload":`, suffix: `}}}`, parents: 5},
	{name: "output-array", field: "output", prefix: `{"kind":"tool_result","output":[`, suffix: `]}`, parents: 4},
}

func legacyNativeAnyFenceTerminal(test *testing.T, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, err, want error) {
	test.Helper()
	if !errors.Is(err, want) || want == context.Canceled && errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("generic native failure lost its error priority", err, want)
	}
	reads := len(input.reads)
	repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
	legacyNativeEventZero(test, repeated)
	if !errors.Is(repeatedErr, want) || want == context.Canceled && errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != reads {
		test.Fatal("generic native terminal failure was not sticky or resumed source reads", repeatedErr, want)
	}
}

func TestLegacyNativeAnyFencesInnerStringRereadFailuresStayAtomic(test *testing.T) {
	long := `"` + strings.Repeat("x", 5*ConversationManifestChunkBytes+73) + `\uD83D\uDE42"`
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, failure := range []string{"part-hash", "short-part", "private-eof", "cancel", "cancel-private-eof", "cancel-part-hash"} {
			test.Run(route.name+"/"+failure, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", route.prefix+long+route.suffix)
				if _, err := legacyNormalizationOracle(raw); err != nil {
					test.Fatal("generic reread fixture is not originally accepted", err)
				}
				input := &legacySourceProofBuffer{body: raw}
				source := legacyNormalizationPreparationInput(test, input, true)
				readSource := source.Read
				last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
				maximumOffset, failedAt := -1, -1
				replayed, triggered := false, false
				var cancelStep context.CancelFunc
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					body, err := readSource(ctx, offset, length)
					if offset < len(raw) && length > 0 {
						replayed = replayed || maximumOffset == last && offset < maximumOffset
						maximumOffset = max(maximumOffset, offset)
					}
					if triggered || !replayed || offset != ConversationManifestChunkBytes || length == 0 {
						return body, err
					}
					triggered, failedAt = true, len(input.reads)
					if strings.HasPrefix(failure, "cancel") {
						cancelStep()
					}
					switch failure {
					case "part-hash", "cancel-part-hash":
						corrupt := bytes.Clone(body)
						corrupt[0] ^= 1
						return corrupt, err
					case "short-part":
						return body[:len(body)-1], err
					case "private-eof", "cancel-private-eof":
						return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
					default:
						return body, err
					}
				}
				constructorCtx, cancelConstructor := context.WithCancel(context.Background())
				normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
				cancelConstructor()
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("generic constructor failed or eagerly read payloads", err, len(input.reads))
				}
				parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
				for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
					stepCtx, stop := context.WithCancel(context.Background())
					cancelStep = stop
					step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
					stop()
					if err == nil {
						if step.Complete || len(step.Events) != 0 {
							test.Fatal("generic reread failure published an event or completed", triggered)
						}
						continue
					}
					want := error(ErrPreparedDocRangeObject)
					if strings.HasPrefix(failure, "cancel") {
						want = context.Canceled
					}
					if !triggered || len(input.reads) != failedAt {
						test.Fatal("generic failure preceded semantic rereading or continued source reads", triggered, err)
					}
					legacyNativeAnyFenceTerminal(test, normalizer, input, err, want)
					return
				}
				test.Fatal("generic reread failure exhausted its fixed per-part progress limit")
			})
		}
	}
}

func TestLegacyNativeAnyFencesRereadsUseCurrentContextAndReleaseSourcePages(test *testing.T) {
	type stepContextKey struct{}
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, discard := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/discard-%t", route.name, discard), func(test *testing.T) {
				prefix := `{"envelope":{"cir_version":"2"},"events":[` + route.prefix
				event := route.prefix + legacyNativeOutputReviewString(test, prefix) + route.suffix
				if discard {
					event = strings.TrimSuffix(event, `}`) + fmt.Sprintf(`,%q:null,%q:{"kept":1}}`, strings.ToUpper(route.field), route.field)
				}
				raw := legacyNativeEventDocument("2", event)
				input := &legacySourceProofBuffer{body: raw}
				source := legacyNormalizationPreparationInput(test, input, true)
				readSource := source.Read
				activeStep, maximumOffset := 0, -1
				replayed, paused, zeroEOF, oneEOF := false, false, false, false
				var borrowed []byte
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					if ctx.Err() != nil || ctx.Value(stepContextKey{}) != activeStep {
						test.Fatal("generic read retained a constructor or earlier step context", offset, length, ctx.Err())
					}
					if offset < len(raw) && length > 0 {
						replayed = replayed || offset < maximumOffset
						maximumOffset = max(maximumOffset, offset)
					}
					zeroEOF = zeroEOF || offset == len(raw) && length == 0
					oneEOF = oneEOF || offset == len(raw) && length == 1
					body, err := readSource(ctx, offset, length)
					if err == nil && len(body) > 0 {
						borrowed = body
					}
					return body, err
				}
				constructorCtx, cancelConstructor := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, -1))
				normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
				cancelConstructor()
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("generic context constructor failed or read payloads", err, len(input.reads))
				}
				var events [][]byte
				parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
				for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
					activeStep++
					stepCtx, cancelStep := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
					step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
					cancelStep()
					clear(borrowed)
					if err != nil {
						test.Fatal("fresh context or poisoned borrowed source broke generic decoding", err)
					}
					if step.Reset {
						events = nil
					}
					for _, event := range step.Events {
						body, err := legacyNormalizationTestBytes(context.Background(), event)
						if err != nil {
							test.Fatal("generic event retained a canceled step context or source page", err)
						}
						events = append(events, body)
					}
					if discard && len(step.Events) != 0 || step.Complete {
						retained, retainedErr := normalizer.RetainedBytes()
						if retainedErr != nil || retained >= 64<<10 {
							test.Fatal("generic overwrite or completion retained giant string work or a source page", retained, retainedErr)
						}
					}
					if replayed && !paused {
						if step.Complete || len(step.Events) != 0 {
							test.Fatal("generic reread did not expose resumable inner string work")
						}
						before, beforeErr := normalizer.RetainedBytes()
						reads := len(input.reads)
						canceled, cancel := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
						cancel()
						interrupted, interruptedErr := legacyNativeScalarReviewStep(test, canceled, normalizer, input)
						legacyNativeEventZero(test, interrupted)
						after, afterErr := normalizer.RetainedBytes()
						if !errors.Is(interruptedErr, context.Canceled) || len(input.reads) != reads || beforeErr != nil || afterErr != nil || before != after {
							test.Fatal("pre-canceled generic step changed reads or retained state", interruptedErr, before, after)
						}
						paused = true
					}
					if !step.Complete {
						continue
					}
					if !replayed || !paused || !zeroEOF || !oneEOF || len(events) != 1 || step.Count != len(events) {
						test.Fatal("generic context fixture lost rereading, resumption, EOF probes, or its event", replayed, paused, zeroEOF, oneEOF, len(events), step.Count)
					}
					var physical int64
					for _, read := range input.reads {
						physical += int64(read[1])
					}
					if physical > 8*int64(len(raw))+2 {
						test.Fatal("generic rereads exceeded the original eightfold source bound", physical, len(raw))
					}
					got := AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(events, []byte(","))})
					legacyNativeScalarReviewCompare(test, raw, got, nil, true)
					return
				}
				test.Fatal("generic context fixture exhausted its fixed per-part progress limit")
			})
		}
	}
}

func TestLegacyNativeAnyFencesEOFAndCancellationStayAtomic(test *testing.T) {
	long := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"`
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, failure := range []struct {
			name   string
			length int
			extra  bool
			noEOF  bool
			cancel bool
		}{
			{name: "zero-private-eof"},
			{name: "zero-extra-byte", extra: true},
			{name: "one-private-eof", length: 1},
			{name: "one-extra-byte", length: 1, extra: true},
			{name: "one-missing-eof", length: 1, noEOF: true},
			{name: "zero-cancel-private-eof", cancel: true},
			{name: "one-cancel-private-eof", length: 1, cancel: true},
		} {
			test.Run(route.name+"/"+failure.name, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", route.prefix+long+route.suffix)
				input := &legacySourceProofBuffer{body: raw}
				source := legacyNormalizationPreparationInput(test, input, true)
				readSource := source.Read
				triggered, failedAt := false, -1
				var cancelStep context.CancelFunc
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					body, err := readSource(ctx, offset, length)
					if triggered || offset != len(raw) || length != failure.length {
						return body, err
					}
					triggered, failedAt = true, len(input.reads)
					if failure.cancel {
						cancelStep()
					}
					if failure.extra {
						return []byte("x"), nil
					}
					if failure.noEOF {
						return nil, nil
					}
					return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
				}
				normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("generic EOF constructor failed or read payloads", err, len(input.reads))
				}
				parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
				for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
					stepCtx, stop := context.WithCancel(context.Background())
					cancelStep = stop
					step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
					stop()
					if err == nil {
						if step.Complete {
							test.Fatal("generic EOF failure produced a completed document", triggered)
						}
						continue
					}
					want := error(ErrPreparedDocRangeObject)
					if failure.cancel {
						want = context.Canceled
					}
					if !triggered || len(input.reads) != failedAt {
						test.Fatal("generic EOF failure continued reading or failed before its probe", triggered, err)
					}
					legacyNativeAnyFenceTerminal(test, normalizer, input, err, want)
					return
				}
				test.Fatal("generic EOF fixture exhausted its fixed per-part progress limit")
			})
		}
	}
}

func TestLegacyNativeAnyFencesDocumentInclusiveDepth(test *testing.T) {
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, totalDepth := range []int{9999, 10000, 10001} {
			test.Run(fmt.Sprintf("%s/depth-%d", route.name, totalDepth), func(test *testing.T) {
				depth := totalDepth - route.parents
				value := strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth)
				raw := legacyNativeEventDocument("2", route.prefix+value+route.suffix)
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, totalDepth <= 10000)
			})
		}
	}
}

func TestLegacyNativeAnyFencesNestedCIRNamesAreDataAndOuterUnknownStaysSkipped(test *testing.T) {
	data := `{"kind":"unknown","seq":"not-a-number","input":[],"Input":{"seq":"bad"},"output":{"provider_metadata":null},"Output":[false,null],"provider_metadata":null,"locked":null,"agent_message":false,"agent_author":"","replacement":[null],"replacement_complete":false}`
	unknown := `,"unknown":{"input":1e999,"output":[1e999],"events":[{"seq":"bad","provider_metadata":null}],"locked":false,"padding":"` + strings.Repeat("x", 5*ConversationManifestChunkBytes+73) + `"}}`
	for _, route := range legacyNativeAnyFenceRoutes {
		test.Run(route.name+"/nested-data-v1", func(test *testing.T) {
			raw := legacyNativeEventDocument("1", route.prefix+data+route.suffix)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
		})
		test.Run(route.name+"/outer-unknown", func(test *testing.T) {
			event := strings.TrimSuffix(route.prefix+`null`+route.suffix, `}`) + unknown
			raw := legacyNativeEventDocument("1", event)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			if nativeSteps, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true); err != nil || nativeSteps < 4 {
				test.Fatal("outer unknown object escaped bounded skipping or decoded nested CIR names", nativeSteps, err)
			}
			normalizer, input = legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
		})
	}
}

func TestLegacyNativeAnyFencesMalformedContainersAndTruncation(test *testing.T) {
	long := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"`
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, fixture := range []struct {
			name  string
			value string
		}{
			{name: "object-trailing-comma", value: `{"payload":` + long + `,}`},
			{name: "array-trailing-comma", value: `[` + long + `,]`},
			{name: "missing-colon", value: `{"payload"` + long + `}`},
			{name: "missing-comma", value: `[` + long + ` true]`},
			{name: "missing-value", value: `{"payload":` + long + `,"other":}`},
			{name: "invalid-escape", value: long[:len(long)-1] + `\q"`},
			{name: "invalid-unicode", value: long[:len(long)-1] + `\u12xz"`},
			{name: "invalid-number", value: `{"payload":` + long + `,"other":01}`},
			{name: "truncated-exponent", value: `[` + long + `,1e+]`},
			{name: "native-overflow-is-not-skipped", value: `{"payload":` + long + `,"input":1e999}`},
		} {
			test.Run(route.name+"/"+fixture.name, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", route.prefix+fixture.value+route.suffix)
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, false)
			})
		}
		raw := legacyNativeEventDocument("2", route.prefix+long+route.suffix)
		for cut := 1; cut <= len(route.suffix)+3; cut++ {
			test.Run(fmt.Sprintf("%s/truncated-%d", route.name, cut), func(test *testing.T) {
				truncated := bytes.Clone(raw[:len(raw)-cut])
				normalizer, input := legacyNativeScalarReviewStart(test, truncated)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, truncated, got, err, false)
			})
		}
	}
}

func TestLegacyNativeAnyFencesWrongInputTopLevelPreservesNativeTypeRejection(test *testing.T) {
	for _, value := range []string{`[]`, `[{"input":[],"output":1e999}]`, `false`, `true`, `7`, `1e999`, `"not-an-object"`} {
		test.Run(value, func(test *testing.T) {
			event := `{"kind":"tool_call","input":` + value + `,"INPUT":{}}`
			var expected, actual wire
			expectedErr := json.Unmarshal([]byte(event), &expected)
			_, actualErr := legacyNativeMetadataDecode(test, []byte(event), &actual, false)
			var expectedTypeError, actualTypeError *json.UnmarshalTypeError
			if !errors.As(expectedErr, &expectedTypeError) || !errors.As(actualErr, &actualTypeError) ||
				actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type ||
				actualTypeError.Type != reflect.TypeFor[map[string]any]() {
				test.Fatal("wrong Input top-level value changed native Value or Type", actualErr, expectedErr)
			}
			raw := legacyNativeEventDocument("2", event)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, false)
		})
	}
}

func TestLegacyNativeAnyFencesCompletedEarlierRejectionDoesNotHidePrivateFault(test *testing.T) {
	long := `"` + strings.Repeat("x", 5*ConversationManifestChunkBytes+73) + `"`
	for _, route := range legacyNativeAnyFenceRoutes {
		for _, failure := range []string{"part-hash", "part-private-eof", "end-zero", "end-one"} {
			test.Run(route.name+"/"+failure, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", `{"kind":"unknown"},`+route.prefix+long+route.suffix)
				if _, err := legacyNormalizationOracle(raw); !errors.Is(err, ErrIntegrity) {
					test.Fatal("completed earlier invalid event lost its original rejection", err)
				}
				input := &legacySourceProofBuffer{body: raw}
				source := legacyNormalizationPreparationInput(test, input, true)
				readSource := source.Read
				last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
				var normalizer *LegacyNormalizationDecoder
				triggered, failedAt := false, -1
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					body, err := readSource(ctx, offset, length)
					selected := (failure == "part-hash" || failure == "part-private-eof") && offset == last && length > 0 ||
						failure == "end-zero" && offset == len(raw) && length == 0 ||
						failure == "end-one" && offset == len(raw) && length == 1
					if triggered || !selected {
						return body, err
					}
					if normalizer == nil || normalizer.count < 1 {
						test.Fatal("private fault preceded completion of the earlier invalid event")
					}
					triggered, failedAt = true, len(input.reads)
					if failure == "part-hash" {
						corrupt := bytes.Clone(body)
						corrupt[0] ^= 1
						return corrupt, err
					}
					return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
				}
				var err error
				normalizer, err = NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil || normalizer == nil || len(input.reads) != 0 {
					test.Fatal("generic error-priority constructor failed or read payloads", err, len(input.reads))
				}
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				if !triggered || got != nil || len(input.reads) != failedAt {
					test.Fatal("earlier rejection hid a private fault, returned bytes, or continued reading", triggered, err)
				}
				legacyNativeAnyFenceTerminal(test, normalizer, input, err, ErrPreparedDocRangeObject)
			})
		}
	}
}
