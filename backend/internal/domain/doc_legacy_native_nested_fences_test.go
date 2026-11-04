package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type legacyNativeNestedFenceField struct {
	name   string
	kind   string
	prefix string
}

var legacyNativeNestedFenceFields = []legacyNativeNestedFenceField{
	{name: "locked", kind: "reasoning", prefix: `{"provider":"codex","scheme":"encrypted_content","blob":`},
	{name: "provider_metadata", kind: "turn", prefix: `{"turn_id":`},
}

func (field legacyNativeNestedFenceField) child(value string) string {
	return field.prefix + value + `}`
}

func (field legacyNativeNestedFenceField) event(child string) string {
	return fmt.Sprintf(`{"kind":%q,%q:%s}`, field.kind, field.name, child)
}

func TestLegacyNativeNestedFencesSelectedRereadFailuresStayAtomic(test *testing.T) {
	long := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"`
	for _, field := range legacyNativeNestedFenceFields {
		for _, failure := range []string{"part-hash", "short-part", "private-eof", "cancel", "cancel-private-eof", "cancel-part-hash"} {
			test.Run(field.name+"/"+failure, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", field.event(field.child(long)))
				if _, err := legacyNormalizationOracle(raw); err != nil {
					test.Fatal("selected nested failure fixture is not originally accepted", err)
				}
				input := &legacySourceProofBuffer{body: raw}
				source := legacyNormalizationPreparationInput(test, input, true)
				readSource := source.Read
				maximumOffset, failedAt := -1, -1
				replayed, triggered := false, false
				var cancelStep context.CancelFunc
				source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
					body, err := readSource(ctx, offset, length)
					if offset < len(raw) && length > 0 {
						replayed = replayed || offset < maximumOffset
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
					test.Fatal("selected nested constructor failed or eagerly read payloads", err, len(input.reads))
				}
				for stepIndex := 0; stepIndex < 128; stepIndex++ {
					stepCtx, stop := context.WithCancel(context.Background())
					cancelStep = stop
					step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
					stop()
					if err == nil {
						if step.Complete || len(step.Events) != 0 {
							test.Fatal("selected nested failure published an event or completed", triggered)
						}
						continue
					}
					want := error(ErrPreparedDocRangeObject)
					if strings.HasPrefix(failure, "cancel") {
						want = context.Canceled
					}
					if !triggered || !errors.Is(err, want) || len(input.reads) != failedAt {
						test.Fatal("selected nested reread lost fault precedence or continued reading", triggered, err, want)
					}
					reads := len(input.reads)
					repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
					legacyNativeEventZero(test, repeated)
					if !errors.Is(repeatedErr, want) || len(input.reads) != reads {
						test.Fatal("selected nested reread terminal failure was not sticky", repeatedErr, want)
					}
					return
				}
				test.Fatal("selected nested reread failure exhausted its fixed progress limit")
			})
		}
	}
}

func TestLegacyNativeNestedFencesSelectedRereadsUseCurrentContext(test *testing.T) {
	type stepContextKey struct{}
	long := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `\uD83D\uDE42"`
	for _, field := range legacyNativeNestedFenceFields {
		test.Run(field.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", field.event(field.child(long)))
			input := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, input, true)
			readSource := source.Read
			activeStep, maximumOffset := 0, -1
			replayed, paused := false, false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if ctx.Err() != nil || ctx.Value(stepContextKey{}) != activeStep {
					test.Fatal("selected nested read retained a constructor or earlier step context", offset, length, ctx.Err())
				}
				if offset < len(raw) && length > 0 {
					replayed = replayed || offset < maximumOffset
					maximumOffset = max(maximumOffset, offset)
				}
				return readSource(ctx, offset, length)
			}
			constructorCtx, cancelConstructor := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, -1))
			normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
			cancelConstructor()
			if err != nil || normalizer == nil || len(input.reads) != 0 {
				test.Fatal("selected nested context constructor failed or read payloads", err, len(input.reads))
			}
			var events [][]byte
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				activeStep++
				stepCtx, cancelStep := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
				step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
				cancelStep()
				if err != nil {
					test.Fatal("fresh step context failed selected nested decoding", err)
				}
				if step.Reset {
					events = nil
				}
				for _, event := range step.Events {
					body, err := legacyNormalizationTestBytes(context.Background(), event)
					if err != nil {
						test.Fatal("selected nested event retained a canceled step context", err)
					}
					events = append(events, body)
				}
				if replayed && !paused {
					if step.Complete || len(step.Events) != 0 {
						test.Fatal("selected nested replay did not expose resumable work")
					}
					before, beforeErr := normalizer.RetainedBytes()
					reads := len(input.reads)
					canceled, cancel := context.WithCancel(context.WithValue(context.Background(), stepContextKey{}, activeStep))
					cancel()
					interrupted, interruptedErr := legacyNativeScalarReviewStep(test, canceled, normalizer, input)
					legacyNativeEventZero(test, interrupted)
					after, afterErr := normalizer.RetainedBytes()
					if !errors.Is(interruptedErr, context.Canceled) || len(input.reads) != reads || beforeErr != nil || afterErr != nil || before != after {
						test.Fatal("pre-canceled selected nested step changed reads or retained state", interruptedErr, before, after)
					}
					paused = true
				}
				if !step.Complete {
					continue
				}
				if !replayed || !paused || len(events) != 1 || step.Count != len(events) {
					test.Fatal("selected nested fixture lost its reread, resumption, or event", replayed, paused, len(events), step.Count)
				}
				var physical int64
				for _, read := range input.reads {
					physical += int64(read[1])
				}
				if physical > 8*int64(len(raw))+2 {
					test.Fatal("selected nested rereads exceeded the original eightfold source bound", physical, len(raw))
				}
				got := AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(events, []byte(","))})
				legacyNativeScalarReviewCompare(test, raw, got, nil, true)
				return
			}
			test.Fatal("selected nested context fixture exhausted its fixed per-part progress limit")
		})
	}
}

func TestLegacyNativeNestedFencesMalformedChildGrammarAndTruncation(test *testing.T) {
	long := `"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"`
	for _, field := range legacyNativeNestedFenceFields {
		child := field.child(long)
		beforeClose := strings.TrimSuffix(child, `}`)
		for _, fixture := range []struct {
			name  string
			child string
		}{
			{name: "child-trailing-comma", child: beforeClose + `,}`},
			{name: "missing-member-colon", child: strings.TrimSuffix(field.prefix, `:`) + long + `}`},
			{name: "missing-member-comma", child: beforeClose + `"unknown":0}`},
			{name: "missing-member-value", child: beforeClose + `,"unknown":}`},
			{name: "repeated-comma", child: beforeClose + `,,"unknown":0}`},
			{name: "unknown-object-trailing-comma", child: beforeClose + `,"unknown":{"nested":true,}}`},
			{name: "unknown-array-trailing-comma", child: beforeClose + `,"unknown":[1,]}`},
			{name: "unknown-invalid-number", child: beforeClose + `,"unknown":01}`},
			{name: "unknown-truncated-exponent", child: beforeClose + `,"unknown":1e+}`},
			{name: "selected-invalid-escape", child: field.prefix + strings.TrimSuffix(long, `"`) + `\q"}`},
			{name: "selected-invalid-unicode", child: field.prefix + strings.TrimSuffix(long, `"`) + `\u12xz"}`},
		} {
			test.Run(field.name+"/"+fixture.name, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", field.event(fixture.child))
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, false)
			})
		}
		raw := legacyNativeEventDocument("2", field.event(child))
		for _, fixture := range []struct {
			name string
			cut  int
		}{
			{name: "truncated-selected-string", cut: 6},
			{name: "truncated-after-selected-string", cut: 4},
			{name: "truncated-after-child", cut: 3},
			{name: "truncated-after-event", cut: 2},
			{name: "truncated-after-events", cut: 1},
		} {
			test.Run(field.name+"/"+fixture.name, func(test *testing.T) {
				truncated := bytes.Clone(raw[:len(raw)-fixture.cut])
				normalizer, input := legacyNativeScalarReviewStart(test, truncated)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, truncated, got, err, false)
			})
		}
	}
}

func TestLegacyNativeNestedFencesHugeUnknownObjectAndJSONDepth(test *testing.T) {
	long := strings.Repeat("x", 5*ConversationManifestChunkBytes+73)
	for _, field := range legacyNativeNestedFenceFields {
		test.Run(field.name+"/huge-unknown-object", func(test *testing.T) {
			child := strings.TrimSuffix(field.child(`"kept"`), `}`) + `,"unknown":{"events":[{"seq":"bad","locked":null,"padding":"` + long + `"}],"turn_id":false,"provider_metadata":null,"agent_message":false,"output":1e999}}`
			raw := legacyNativeEventDocument("2", field.event(child))
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			if nativeSteps, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true); err != nil || nativeSteps < 4 {
				test.Fatal("unknown nested object escaped bounded cooperative decoding", nativeSteps, err)
			}
			normalizer, input = legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
		})
		for _, depth := range []int{9995, 9996, 9997} {
			test.Run(fmt.Sprintf("%s/document-inclusive-depth-%d", field.name, depth+4), func(test *testing.T) {
				unknown := strings.Repeat(`{"ignored":`, depth) + `"` + long + `"` + strings.Repeat(`}`, depth)
				child := strings.TrimSuffix(field.child(`"kept"`), `}`) + `,"unknown":` + unknown + `}`
				raw := legacyNativeEventDocument("2", field.event(child))
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, depth+4 <= 10000)
			})
		}
	}
}

func TestLegacyNativeNestedFencesWrongKindNativeValueAndType(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		member string
	}{
		{name: "locked-provider", member: `"locked":{"provider":%s}`},
		{name: "locked-scheme", member: `"locked":{"scheme":%s}`},
		{name: "locked-blob", member: `"locked":{"blob":%s}`},
		{name: "metadata-turn-id", member: `"provider_metadata":{"turn_id":%s}`},
		{name: "metadata-create-time", member: `"provider_metadata":{"create_time":%s}`},
	} {
		for _, value := range []string{`false`, `7`, `[]`, `{}`} {
			if fixture.name == "metadata-create-time" && value == `7` {
				continue
			}
			test.Run(fixture.name+"/"+value, func(test *testing.T) {
				event := `{"kind":"reasoning",` + fmt.Sprintf(fixture.member, value) + `}`
				var actual, expected wire
				expectedErr := json.Unmarshal([]byte(event), &expected)
				_, actualErr := legacyNativeMetadataDecode(test, []byte(event), &actual, false)
				var actualTypeError, expectedTypeError *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedTypeError) || !errors.As(actualErr, &actualTypeError) ||
					actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
					test.Fatal("selected nested wrong-kind rejection changed native Value or Type", actualErr, expectedErr)
				}
				raw := legacyNativeEventDocument("2", event)
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, false)
			})
		}
	}
	for _, field := range legacyNativeNestedFenceFields {
		for _, value := range []string{`false`, `7`, `"not-an-object"`, `[]`} {
			test.Run(field.name+"/child-"+value, func(test *testing.T) {
				event := field.event(value)
				var actual, expected wire
				expectedErr := json.Unmarshal([]byte(event), &expected)
				_, actualErr := legacyNativeMetadataDecode(test, []byte(event), &actual, false)
				var actualTypeError, expectedTypeError *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedTypeError) || !errors.As(actualErr, &actualTypeError) ||
					actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
					test.Fatal("selected child wrong-kind rejection changed native Value or Type", actualErr, expectedErr)
				}
				raw := legacyNativeEventDocument("2", event)
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, false)
			})
		}
	}
}

func TestLegacyNativeNestedFencesLaterPrivateFaultOutranksEarlierRejection(test *testing.T) {
	long := `"` + strings.Repeat("x", 5*ConversationManifestChunkBytes+73) + `"`
	for _, field := range legacyNativeNestedFenceFields {
		for _, fixture := range []struct {
			name   string
			events string
		}{
			{name: "native-child-type", events: field.event(strings.TrimSuffix(field.child(`false`), `}`) + `,"unknown":` + long + `}`)},
			{name: "earlier-event-union", events: `{"kind":"unknown"},` + field.event(field.child(long))},
			{name: "earlier-event-null-metadata", events: `{"kind":"turn","provider_metadata":null},` + field.event(field.child(long))},
		} {
			for _, failure := range []string{"part-hash", "part-private-eof", "end-zero", "end-one"} {
				test.Run(field.name+"/"+fixture.name+"/"+failure, func(test *testing.T) {
					raw := legacyNativeEventDocument("2", fixture.events)
					if _, err := legacyNormalizationOracle(raw); !errors.Is(err, ErrIntegrity) {
						test.Fatal("earlier nested semantic rejection fixture is originally accepted", err)
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
						knownRejection := normalizer != nil && (normalizer.invalid || normalizer.phase == legacyNormalizationReject)
						if normalizer != nil && normalizer.native != nil {
							for _, child := range normalizer.native.object.nested {
								knownRejection = knownRejection || child != nil && child.typeError != nil
							}
						}
						if !knownRejection {
							test.Fatal("later private fault fired before the earlier native or semantic rejection")
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
						test.Fatal("nested priority constructor failed or read payloads", err, len(input.reads))
					}
					got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
					if !triggered || !errors.Is(err, ErrPreparedDocRangeObject) || got != nil || len(input.reads) != failedAt {
						test.Fatal("earlier nested rejection hid a later private fault or continued reading", triggered, err)
					}
					reads := len(input.reads)
					repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
					legacyNativeEventZero(test, repeated)
					if !errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != reads {
						test.Fatal("nested rejection lost sticky private-source failure", repeatedErr)
					}
				})
			}
		}
	}
}
