package domain

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func legacyNativePublicRejectRoutes() []struct {
	name   string
	prefix string
	suffix string
} {
	return []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "output-root", prefix: `{"kind":"tool_result","seq":2,"output":`, suffix: `}`},
		{name: "output-object", prefix: `{"kind":"tool_result","seq":2,"output":{"outer":[false,{"number":`, suffix: `},null]}}`},
		{name: "output-array", prefix: `{"kind":"tool_result","seq":2,"output":[false,{"number":`, suffix: `},null]}`},
		{name: "input-map-value", prefix: `{"kind":"tool_call","seq":2,"input":{"number":`, suffix: `}}`},
		{name: "input-nested", prefix: `{"kind":"tool_call","seq":2,"input":{"outer":[false,{"number":`, suffix: `},null]}}`},
	}
}

func legacyNativePublicRejectDocument(event string) []byte {
	return []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":1,"id":"provisional"},` + event +
		`],"ignored":{"tail":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}}`)
}

func TestLegacyNativePublicRejectFencesOverflowDrainsBeforeTerminalFailure(test *testing.T) {
	overflow := "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+73)
	for _, route := range legacyNativePublicRejectRoutes() {
		test.Run(route.name, func(test *testing.T) {
			raw := legacyNativePublicRejectDocument(route.prefix + overflow + route.suffix)
			if _, err := legacyNormalizationOracle(raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("public overflow fixture lost its original typed rejection", err)
			}
			for _, failure := range []struct {
				name      string
				damage    string
				atEOF     bool
				eofLength int
				cancel    bool
			}{
				{name: "authenticated-rejection"},
				{name: "late-part-hash", damage: "hash"},
				{name: "late-short-part", damage: "short"},
				{name: "late-part-cancel", cancel: true},
				{name: "late-part-hash-cancel", damage: "hash", cancel: true},
				{name: "late-short-part-cancel", damage: "short", cancel: true},
				{name: "zero-private-eof", damage: "private-eof", atEOF: true},
				{name: "one-private-eof", damage: "private-eof", atEOF: true, eofLength: 1},
				{name: "one-missing-eof", damage: "missing-eof", atEOF: true, eofLength: 1},
				{name: "zero-private-eof-cancel", damage: "private-eof", atEOF: true, cancel: true},
				{name: "one-private-eof-cancel", damage: "private-eof", atEOF: true, eofLength: 1, cancel: true},
				{name: "one-missing-eof-cancel", damage: "missing-eof", atEOF: true, eofLength: 1, cancel: true},
			} {
				test.Run(failure.name, func(test *testing.T) {
					input := &legacySourceProofBuffer{body: raw}
					source := legacyNormalizationPreparationInput(test, input, true)
					readSource := source.Read
					last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
					var normalizer *LegacyNormalizationDecoder
					var cancelStep context.CancelFunc
					drainedParts, failedAt := 0, -1
					var probes [2]int
					triggered, lateReadBeforeReject := false, false
					faulty := failure.damage != "" || failure.cancel
					source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						body, err := readSource(ctx, offset, length)
						if normalizer == nil || normalizer.phase != legacyNormalizationReject {
							lateReadBeforeReject = lateReadBeforeReject || offset == last && length > 0
							return body, err
						}
						if offset < len(raw) && length > 0 {
							if offset != drainedParts*ConversationManifestChunkBytes {
								test.Fatal("public numeric rejection skipped or reordered an original source part", drainedParts, offset, length)
							}
							drainedParts++
						} else if offset == len(raw) && (length == 0 || length == 1) {
							if drainedParts != len(source.Parts) {
								test.Fatal("public numeric rejection probed EOF before draining every original part", drainedParts, len(source.Parts))
							}
							probes[length]++
						}
						matches := failure.atEOF && offset == len(raw) && length == failure.eofLength ||
							!failure.atEOF && faulty && offset == last && length > 0
						if triggered || !matches {
							return body, err
						}
						triggered, failedAt = true, len(input.reads)
						if failure.cancel {
							cancelStep()
						}
						switch failure.damage {
						case "hash":
							corrupt := append([]byte(nil), body...)
							corrupt[0] ^= 1
							return corrupt, err
						case "short":
							return body[:len(body)-1], err
						case "private-eof":
							return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
						case "missing-eof":
							return nil, nil
						default:
							return body, err
						}
					}
					constructorCtx, cancelConstructor := context.WithCancel(context.Background())
					var err error
					normalizer, err = NewLegacyNormalizationDecoder(constructorCtx, source)
					cancelConstructor()
					if err != nil || normalizer == nil || len(input.reads) != 0 {
						test.Fatal("public numeric constructor failed or eagerly read payloads", err, len(input.reads))
					}
					paused := false
					for stepIndex := 0; stepIndex < 64+32*len(source.Parts); stepIndex++ {
						stepCtx, stop := context.WithCancel(context.Background())
						cancelStep = stop
						step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
						stop()
						if err == nil {
							if step.Complete {
								test.Fatal("public numeric overflow completed before its required rejection", triggered)
							}
							if !paused && normalizer.phase == legacyNormalizationReject {
								reads, part := len(input.reads), normalizer.rejectPart
								canceled, cancel := context.WithCancel(context.Background())
								cancel()
								interrupted, interruptedErr := legacyNativeScalarReviewStep(test, canceled, normalizer, input)
								legacyNativeEventZero(test, interrupted)
								if !errors.Is(interruptedErr, context.Canceled) || errors.Is(interruptedErr, ErrIntegrity) ||
									len(input.reads) != reads || normalizer.rejectPart != part || normalizer.phase != legacyNormalizationReject {
									test.Fatal("pre-canceled public rejection changed its drain or lost context precedence", interruptedErr)
								}
								paused = true
							}
							continue
						}
						legacyNativeEventZero(test, step)
						want := error(ErrIntegrity)
						private := faulty && !failure.cancel
						if failure.cancel {
							want = context.Canceled
						} else if private {
							want = ErrPreparedDocRangeObject
						}
						if !paused || lateReadBeforeReject || triggered != faulty || drainedParts != len(source.Parts) ||
							faulty && len(input.reads) != failedAt {
							test.Fatal("public overflow bypassed rejection draining, hid a late fault, or kept reading after failure", paused, lateReadBeforeReject, triggered, drainedParts, err)
						}
						wantProbes := [2]int{1, 1}
						if faulty && !failure.atEOF {
							wantProbes = [2]int{}
						} else if failure.atEOF && failure.eofLength == 0 {
							wantProbes = [2]int{1, 0}
						}
						if probes != wantProbes {
							test.Fatal("public overflow skipped a required EOF probe or probed after failure", probes, wantProbes)
						}
						for attempt := 0; attempt < 2; attempt++ {
							if !errors.Is(err, want) || errors.Is(err, ErrPreparedDocRangeObject) != private ||
								errors.Is(err, ErrIntegrity) != !failure.cancel || errors.Is(err, context.Canceled) != failure.cancel {
								test.Fatal("public numeric rejection lost authentication, EOF, or context precedence", err, want)
							}
							if attempt == 0 {
								reads := len(input.reads)
								step, err = legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
								legacyNativeEventZero(test, step)
								if len(input.reads) != reads {
									test.Fatal("terminal public numeric rejection resumed source reads")
								}
							}
						}
						return
					}
					test.Fatal("public numeric rejection exhausted its finite per-part progress allowance")
				})
			}
		})
	}
}

func TestLegacyNativePublicRejectFencesAcceptedGiantNumbers(test *testing.T) {
	digits := 2*ConversationManifestChunkBytes + 73
	for _, token := range []struct {
		name string
		raw  string
	}{
		{name: "negative-zero", raw: "-0e+" + strings.Repeat("9", digits)},
		{name: "negative-underflow", raw: "-1e-" + strings.Repeat("9", digits)},
		{name: "finite-zero-padded-exponent", raw: "1e+" + strings.Repeat("0", digits) + "1"},
	} {
		for _, route := range legacyNativePublicRejectRoutes() {
			test.Run(route.name+"/"+token.name, func(test *testing.T) {
				raw := legacyNativeEventDocument("2", route.prefix+token.raw+route.suffix)
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
				legacyNativeScalarReviewCompare(test, raw, got, err, true)
			})
		}
	}
}

func TestLegacyNativePublicRejectFencesUnknownGiantOverflowStaysIgnored(test *testing.T) {
	overflow := "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+73)
	for _, fixture := range []struct {
		name string
		raw  []byte
	}{
		{name: "document-field", raw: []byte(`{"ignored":` + overflow + `,"envelope":{"cir_version":"2"},"events":[{"kind":"turn"}]}`)},
		{name: "envelope-field", raw: []byte(`{"envelope":{"cir_version":"2","ignored":` + overflow + `},"events":[{"kind":"turn"}]}`)},
		{name: "event-field", raw: legacyNativeEventDocument("2", `{"kind":"turn","ignored":`+overflow+`}`)},
		{name: "unknown-nested-cir-names", raw: legacyNativeEventDocument("2", `{"kind":"turn","ignored":{"input":[`+overflow+`],"output":`+overflow+`}}`)},
		{name: "provider-unknown-field", raw: legacyNativeEventDocument("2", `{"kind":"turn","provider_metadata":{"ignored":`+overflow+`}}`)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			normalizer, input := legacyNativeScalarReviewStart(test, fixture.raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, fixture.raw, got, err, true)
		})
	}
}
