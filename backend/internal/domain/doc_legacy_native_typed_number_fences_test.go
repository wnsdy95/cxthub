package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type legacyNativeTypedNumberFenceToken struct {
	name     string
	raw      string
	lexeme   string
	rejected bool
}

func legacyNativeTypedNumberFenceTokens() []legacyNativeTypedNumberFenceToken {
	digits := 2*ConversationManifestChunkBytes + 73
	zeroes := strings.Repeat("0", digits)
	escapedDigits := digits/6 + 1
	escapedZeroes := strings.Repeat(`\u0030`, escapedDigits)
	raw := "1" + zeroes + "E+03"
	quoted := "-0." + zeroes + "E+0003"
	return []legacyNativeTypedNumberFenceToken{
		{name: "raw-beyond-float64", raw: raw, lexeme: raw},
		{name: "quoted-negative-zero", raw: `"` + quoted + `"`, lexeme: quoted},
		{name: "escaped-beyond-float64", raw: `"\u002d1\u002e` + escapedZeroes + `\u0045\u002b9999"`, lexeme: "-1." + strings.Repeat("0", escapedDigits) + "E+9999"},
		{name: "quoted-leading-zero", raw: `"0` + strings.Repeat("1", digits) + `"`, rejected: true},
		{name: "quoted-missing-exponent", raw: `"1` + zeroes + `E+"`, rejected: true},
		{name: "escaped-missing-exponent", raw: `"1` + escapedZeroes + `\u0045\u002b"`, rejected: true},
	}
}

func legacyNativeTypedNumberFenceDocument(token legacyNativeTypedNumberFenceToken) []byte {
	return []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":1,"provider_metadata":{"create_time":` + token.raw +
		`,"turn_id":"after-number"}}],"ignored":{"tail":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}}`)
}

func legacyNativeTypedNumberFenceObserve(test *testing.T, decoder *LegacyNormalizationDecoder, buffer *legacySourceProofBuffer, before int) int {
	test.Helper()
	loads := 0
	for _, read := range buffer.reads[before:] {
		if read[0] == len(buffer.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(buffer.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(buffer.body)-read[0]) {
			test.Fatal("typed number bypassed authenticated full-part reads", read)
		}
		loads++
	}
	retained, err := decoder.RetainedBytes()
	if loads > 2 || err != nil || retained <= 0 || retained > 16<<20 {
		test.Fatal("typed number escaped the two-load or parked heap gate", loads, retained, err)
	}
	return loads
}

func legacyNativeTypedNumberFenceCoverage(test *testing.T, buffer *legacySourceProofBuffer, rejected bool) {
	test.Helper()
	visited := make([]bool, (len(buffer.body)+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes)
	var probes [2]int
	var physical int64
	for _, read := range buffer.reads {
		physical += int64(read[1])
		if read[0] == len(buffer.body) {
			probes[read[1]]++
		} else {
			visited[read[0]/ConversationManifestChunkBytes] = true
		}
	}
	for ordinal, covered := range visited {
		if !covered {
			test.Fatal("typed number omitted an original source part", ordinal)
		}
	}
	if physical < int64(len(buffer.body)) || physical > 8*int64(len(buffer.body))+2 || probes[0] == 0 || probes[1] == 0 ||
		rejected && probes != [2]int{1, 1} {
		test.Fatal("typed number did not consume the full source and both EOF probes within linear reads", physical, probes)
	}
}

func TestLegacyNativeTypedNumberFencesPublicLexicalParity(test *testing.T) {
	if ConversationManifestChunkBytes != 512<<10 || canonicalCheckBytes != 64<<10 {
		test.Fatal("typed number fences require the original source and helper quanta")
	}
	for _, token := range legacyNativeTypedNumberFenceTokens() {
		test.Run(token.name, func(test *testing.T) {
			raw := legacyNativeTypedNumberFenceDocument(token)
			if len(token.raw) <= 2*ConversationManifestChunkBytes || !json.Valid(raw) {
				test.Fatal("typed number fixture lost its multi-part extent or valid JSON syntax")
			}
			buffer := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, buffer, true)
			readSource := source.Read
			var decoder *LegacyNormalizationDecoder
			drainedParts := 0
			last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if token.rejected {
					if offset == last && length > 0 && decoder.phase != legacyNormalizationReject {
						test.Fatal("bad quoted number read the late tail before semantic rejection")
					}
					if decoder.phase == legacyNormalizationReject && offset < len(raw) && length > 0 {
						if offset != drainedParts*ConversationManifestChunkBytes {
							test.Fatal("bad quoted number skipped or reordered the rejection drain", drainedParts, offset)
						}
						drainedParts++
					}
					if offset == len(raw) && drainedParts != len(source.Parts) {
						test.Fatal("bad quoted number probed EOF before authenticating the complete rejection drain")
					}
				}
				return readSource(ctx, offset, length)
			}
			constructorCtx, cancel := context.WithCancel(context.Background())
			var err error
			decoder, err = NewLegacyNormalizationDecoder(constructorCtx, source)
			cancel()
			if err != nil || decoder == nil || len(buffer.reads) != 0 {
				test.Fatal("typed number constructor failed or eagerly read source bytes", err)
			}
			got, err := legacyNativeScalarReviewDrain(test, decoder, buffer)
			legacyNativeScalarReviewCompare(test, raw, got, err, !token.rejected)
			legacyNativeTypedNumberFenceObserve(test, decoder, buffer, len(buffer.reads))
			legacyNativeTypedNumberFenceCoverage(test, buffer, token.rejected)
			if token.rejected {
				if err != ErrIntegrity || got != nil || drainedParts != len(source.Parts) {
					test.Fatal("public bad quoted number did not finish its authenticated, diagnostic-free rejection")
				}
				return
			}
			var decoded struct {
				Events []wire `json:"events"`
			}
			if decodeErr := json.Unmarshal(got, &decoded); decodeErr != nil || len(decoded.Events) != 1 ||
				decoded.Events[0].ProviderMetadata == nil || decoded.Events[0].ProviderMetadata.CreateTime == nil {
				test.Fatal("canonical output lost the accepted provider number", decodeErr)
			}
			metadata := decoded.Events[0].ProviderMetadata
			if metadata.CreateTime.String() != token.lexeme || metadata.TurnID != "after-number" ||
				!bytes.Contains(got, []byte(`"create_time":`+token.lexeme)) {
				test.Fatal("accepted json.Number lost its raw spelling, became quoted, or took the float64 overflow route")
			}
		})
	}
}

func TestLegacyNativeTypedNumberFencesActualSharedWork(test *testing.T) {
	var defaults legacyNormalizationInput
	if defaults.numberDiagnostics != legacyNativeNumberDiagnosticsFull || legacyNativeNumberDiagnosticsFull != 0 {
		test.Fatal("public typed number work changed the separate private default diagnostics policy")
	}
	for _, token := range legacyNativeTypedNumberFenceTokens() {
		test.Run(token.name, func(test *testing.T) {
			raw := legacyNativeTypedNumberFenceDocument(token)
			decoder, buffer := legacyNativeScalarReviewStart(test, raw)
			for stepIndex := 0; stepIndex < 64+32*len(decoder.source.Parts); stepIndex++ {
				initialWork, initialLoads := 0, 0
				if stepIndex%2 != 0 {
					initialWork, initialLoads = 73, 1
				}
				ctx, cancel := context.WithCancel(context.Background())
				input := &legacyNormalizationInput{
					ctx: ctx, source: decoder.source, workBytes: initialWork, loads: initialLoads,
					numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly,
				}
				before := len(buffer.reads)
				step, failure := decoder.scan(ctx, input)
				cancel()
				loads := legacyNativeTypedNumberFenceObserve(test, decoder, buffer, before)
				if input.unbounded || input.failure != nil || input.workBytes < initialWork || input.workBytes > ConversationManifestChunkBytes ||
					input.loads < initialLoads || input.loads > 2 || input.loads-initialLoads != loads {
					test.Fatal("typed number reset or exceeded actual enclosing work or load counters", initialWork, input.workBytes, initialLoads, input.loads, loads)
				}
				if failure != nil {
					legacyNativeEventZero(test, step)
					var semantic *json.UnmarshalTypeError
					if !token.rejected || errors.As(failure, &semantic) || len(failure.Error()) > 128 ||
						errors.Is(failure, ErrPreparedDocRangeObject) || errors.Is(failure, context.Canceled) {
						test.Fatalf("RejectOnly typed number built diagnostic text or changed acceptance: type=%T bytes=%d", failure, len(failure.Error()))
					}
					return
				}
				if decoder.phase == legacyNormalizationFinalizing {
					if token.rejected || decoder.count != 1 {
						test.Fatal("shared typed number route accepted a bad quoted string or lost its event")
					}
					return
				}
			}
			test.Fatal("typed number exhausted its finite shared-work schedule")
		})
	}
}

func TestLegacyNativeTypedNumberFencesLateSourceAndContextPrecedence(test *testing.T) {
	tokens := legacyNativeTypedNumberFenceTokens()
	privateEOF := errors.New("typed-number-private-eof")
	for _, failure := range []struct {
		name      string
		token     int
		damage    string
		atEOF     bool
		eofLength int
		cancel    bool
	}{
		{name: "accepted-raw-late-hash", token: 0, damage: "hash"},
		{name: "accepted-quoted-late-short", token: 1, damage: "short"},
		{name: "accepted-escaped-private-eof", token: 2, damage: "private-eof", atEOF: true, eofLength: 1},
		{name: "rejected-quoted-late-hash", token: 4, damage: "hash"},
		{name: "rejected-escaped-late-short", token: 5, damage: "short"},
		{name: "rejected-quoted-zero-private-eof", token: 4, damage: "private-eof", atEOF: true},
		{name: "rejected-escaped-one-private-eof", token: 5, damage: "private-eof", atEOF: true, eofLength: 1},
		{name: "rejected-quoted-late-hash-cancel", token: 4, damage: "hash", cancel: true},
		{name: "rejected-escaped-private-eof-cancel", token: 5, damage: "private-eof", atEOF: true, eofLength: 1, cancel: true},
	} {
		test.Run(failure.name, func(test *testing.T) {
			token := tokens[failure.token]
			raw := legacyNativeTypedNumberFenceDocument(token)
			buffer := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, buffer, true)
			readSource := source.Read
			last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
			var decoder *LegacyNormalizationDecoder
			var cancelStep context.CancelFunc
			var probes [2]int
			drainedParts, failedAt := 0, -1
			triggered, paused := false, false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				if token.rejected && decoder.phase == legacyNormalizationReject && offset < len(raw) && length > 0 {
					if offset != drainedParts*ConversationManifestChunkBytes {
						test.Fatal("typed rejection skipped or reordered an original source part", drainedParts, offset)
					}
					drainedParts++
				}
				if offset == len(raw) && (length == 0 || length == 1) {
					if token.rejected && drainedParts != len(source.Parts) {
						test.Fatal("typed rejection reached EOF before its complete source drain")
					}
					probes[length]++
				}
				matches := failure.atEOF && offset == len(raw) && length == failure.eofLength ||
					!failure.atEOF && offset == last && length > 0
				if triggered || !matches {
					return body, err
				}
				if token.rejected && decoder.phase != legacyNormalizationReject {
					test.Fatal("late source failure ran before the quoted number's semantic rejection")
				}
				triggered, failedAt = true, len(buffer.reads)
				if failure.cancel {
					cancelStep()
				}
				switch failure.damage {
				case "hash":
					corrupt := bytes.Clone(body)
					corrupt[0] ^= 1
					return corrupt, err
				case "short":
					return body[:len(body)-1], err
				default:
					return nil, errors.Join(ErrPreparedDocRangeObject, privateEOF, io.EOF)
				}
			}
			constructorCtx, cancel := context.WithCancel(context.Background())
			var err error
			decoder, err = NewLegacyNormalizationDecoder(constructorCtx, source)
			cancel()
			if err != nil || decoder == nil || len(buffer.reads) != 0 {
				test.Fatal("late typed-number constructor failed or eagerly read source bytes", err)
			}
			for stepIndex := 0; stepIndex < 64+32*len(source.Parts); stepIndex++ {
				stepCtx, stop := context.WithCancel(context.Background())
				cancelStep = stop
				before := len(buffer.reads)
				step, err := legacyNativeScalarReviewStep(test, stepCtx, decoder, buffer)
				stop()
				legacyNativeTypedNumberFenceObserve(test, decoder, buffer, before)
				if err == nil {
					if step.Complete {
						test.Fatal("typed number completed despite its required late source failure", triggered)
					}
					if token.rejected && !paused && decoder.phase == legacyNormalizationReject {
						reads, part := len(buffer.reads), decoder.rejectPart
						retained, _ := decoder.RetainedBytes()
						canceled, cancel := context.WithCancel(context.Background())
						cancel()
						interrupted, interruptedErr := legacyNativeScalarReviewStep(test, canceled, decoder, buffer)
						legacyNativeEventZero(test, interrupted)
						after, _ := decoder.RetainedBytes()
						if interruptedErr != context.Canceled || len(buffer.reads) != reads || decoder.rejectPart != part ||
							decoder.phase != legacyNormalizationReject || retained != after {
							test.Fatal("pre-canceled typed rejection mutated its resumable source drain", interruptedErr)
						}
						paused = true
					}
					continue
				}
				legacyNativeEventZero(test, step)
				if !triggered || len(buffer.reads) != failedAt || token.rejected && (!paused || drainedParts != len(source.Parts)) {
					test.Fatal("typed number hid a late fault, skipped source draining, or read after failure", triggered, drainedParts, err)
				}
				wantProbes := [2]int{}
				if failure.atEOF {
					wantProbes = [2]int{1, failure.eofLength}
				}
				if probes != wantProbes {
					test.Fatal("typed number skipped a required EOF probe or probed after failure", probes, wantProbes)
				}
				want := error(ErrPreparedDocRangeObject)
				if failure.cancel {
					want = context.Canceled
				}
				for attempt := 0; attempt < 2; attempt++ {
					if !errors.Is(err, want) || errors.Is(err, ErrIntegrity) != !failure.cancel || errors.Is(err, context.Canceled) != failure.cancel ||
						errors.Is(err, ErrPreparedDocRangeObject) != !failure.cancel ||
						errors.Is(err, privateEOF) != (failure.atEOF && !failure.cancel) {
						test.Fatal("typed number lost private-source or context precedence", err, want)
					}
					if attempt == 0 {
						step, err = legacyNativeScalarReviewStep(test, context.Background(), decoder, buffer)
						legacyNativeEventZero(test, step)
						if len(buffer.reads) != failedAt {
							test.Fatal("terminal typed-number failure resumed source reads")
						}
					}
				}
				return
			}
			test.Fatal("typed number exhausted its finite late-failure schedule")
		})
	}
}
