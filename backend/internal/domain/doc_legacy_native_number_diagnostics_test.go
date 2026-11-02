package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeNumberDiagnosticsFixture struct {
	number *legacyNativeNumberValue
	buffer *legacySourceProofBuffer
	source LegacyByteSource
}

func legacyNativeNumberDiagnosticsStart(test *testing.T, token string, from int, diagnostics legacyNativeNumberDiagnostics) *legacyNativeNumberDiagnosticsFixture {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", from) + token + ",false]")}
	number, err := newLegacyNativeNumberValue(from, from+len(token), diagnostics)
	if err != nil || number == nil || number.parser == nil {
		test.Fatal("numeric diagnostics constructor rejected a valid span", err)
	}
	if number.diagnostics != diagnostics || number.parser.position != from || len(buffer.reads) != 0 {
		test.Fatal("numeric diagnostics constructor lost policy, position, or source laziness")
	}
	return &legacyNativeNumberDiagnosticsFixture{number: number, buffer: buffer, source: buffer.input()}
}

func (fixture *legacyNativeNumberDiagnosticsFixture) step(test *testing.T, ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	test.Helper()
	beforePosition, beforeWork := fixture.number.parser.position, input.workBytes
	beforeLoads, beforeReads := input.loads, len(fixture.buffer.reads)
	complete, err := fixture.number.step(ctx, input)
	if input.unbounded || input.workBytes < beforeWork || input.workBytes > ConversationManifestChunkBytes ||
		input.loads < beforeLoads || input.loads > 2 || fixture.number.parser.position < beforePosition ||
		fixture.number.parser.position > fixture.number.parser.end || fixture.number.parser.position-beforePosition > input.workBytes-beforeWork {
		test.Fatal("numeric diagnostics escaped the shared work, load, or selected-span budget")
	}
	reads := fixture.buffer.reads[beforeReads:]
	if len(reads) > 2-beforeLoads || input.failure == nil && len(reads) != input.loads-beforeLoads {
		test.Fatal("numeric diagnostics introduced a private source-load allowance", reads, input.loads-beforeLoads)
	}
	for _, read := range reads {
		if read[0] < 0 || read[0] >= fixture.source.Bytes || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, fixture.source.Bytes-read[0]) {
			test.Fatal("numeric diagnostics bypassed authenticated source-part reads", read)
		}
	}
	if fixture.number.diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
		if fixture.number.errorText != nil || fixture.number.semantic != nil {
			test.Fatal("reject-only numeric diagnostics retained raw error text or a fabricated type error")
		}
		var retained, maximum docRangeHeaderHeapEstimate
		fixture.number.retained(&retained)
		maximum.allocation(1, int64(reflect.TypeOf(*fixture.number).Size()))
		maximum.allocation(1, int64(reflect.TypeFor[legacyNativeFloat64]().Size()))
		maximum.allocation(1, int64(reflect.TypeFor[legacyNativeFloat64Decimal]().Size()))
		if retained.err != nil || maximum.err != nil || retained.bytes <= 0 || retained.bytes > maximum.bytes {
			test.Fatal("reject-only numeric diagnostics exceeded fixed reducer ownership", retained, maximum)
		}
	}
	return complete, err
}

func (fixture *legacyNativeNumberDiagnosticsFixture) drain(test *testing.T) (int, error) {
	test.Helper()
	semanticBytes := 0
	for range 256 {
		ctx := context.Background()
		input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
		if fixture.number.diagnostics != legacyNativeNumberDiagnosticsFull {
			input.numberDiagnostics = fixture.number.diagnostics
		}
		complete, err := fixture.step(test, ctx, input)
		semanticBytes += input.semanticBytes
		if input.failure != nil {
			return semanticBytes, input.failure
		}
		if err != nil || complete {
			return semanticBytes, err
		}
	}
	test.Fatal("numeric diagnostics exhausted the finite scan/conversion/diagnostic schedule")
	return semanticBytes, ErrIntegrity
}

func TestLegacyNativeNumberDiagnosticsDefaultFullExactOverflow(test *testing.T) {
	var defaults legacyNormalizationInput
	if legacyNativeNumberDiagnosticsFull != 0 || defaults.numberDiagnostics != legacyNativeNumberDiagnosticsFull {
		test.Fatal("private zero-value input no longer selects full numeric diagnostics")
	}
	for _, token := range []string{
		"1e309",
		"-1.7976931348623159E+308",
		"1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17),
		"-1e+" + strings.Repeat("9", ConversationManifestChunkBytes+17),
	} {
		for _, from := range []int{0, 17, ConversationManifestChunkBytes - 2} {
			test.Run(fmt.Sprintf("bytes-%d/from-%d", len(token), from), func(test *testing.T) {
				var expected any
				oracleErr := json.Unmarshal([]byte(token), &expected)
				var oracle *json.UnmarshalTypeError
				if !errors.As(oracleErr, &oracle) || oracle.Value != "number "+token || oracle.Offset != int64(len(token))+1 {
					test.Fatal("overflow fixture lost its native numeric oracle", oracleErr)
				}
				fixture := legacyNativeNumberDiagnosticsStart(test, token, from, defaults.numberDiagnostics)
				semanticBytes, err := fixture.drain(test)
				var actual *json.UnmarshalTypeError
				if !errors.As(err, &actual) || actual != fixture.number.semantic || !reflect.DeepEqual(actual, oracle) || err.Error() != oracleErr.Error() {
					test.Fatal("full numeric diagnostics changed raw spelling, float64 type, token-relative offset, or error text")
				}
				if !fixture.number.complete || !fixture.number.converted || !fixture.number.rejected || fixture.number.errorText == nil ||
					fixture.number.parser.position != from+len(token) || semanticBytes != 2*len(token) {
					test.Fatal("full numeric diagnostics did not finish one conversion and one exact raw-text pass", semanticBytes)
				}
			})
		}
	}
}

func TestLegacyNativeNumberDiagnosticsRejectOnlyFixedOverflow(test *testing.T) {
	for _, length := range []int{1, LegacyCanonicalEncodingBytes + 17, ConversationManifestChunkBytes + 17, 2*ConversationManifestChunkBytes + 17} {
		for _, exponent := range []bool{false, true} {
			test.Run(fmt.Sprintf("bytes-%d/exponent-%t", length, exponent), func(test *testing.T) {
				token := "-1" + strings.Repeat("0", length) + "e309"
				if exponent {
					token = "-1E+" + strings.Repeat("9", max(3, length))
				}
				fixture := legacyNativeNumberDiagnosticsStart(test, token, 17, legacyNativeNumberDiagnosticsRejectOnly)
				semanticBytes, err := fixture.drain(test)
				var semantic *json.UnmarshalTypeError
				if err != errLegacyNativeNumberRejected || errors.As(err, &semantic) || len(err.Error()) > 64 || strings.Contains(err.Error(), token) {
					test.Fatal("reject-only overflow did not return the small private rejection sentinel")
				}
				if !fixture.number.complete || !fixture.number.converted || !fixture.number.rejected ||
					fixture.number.parser.position != 17+len(token) || semanticBytes != len(token) {
					test.Fatal("reject-only overflow replayed raw text or rejected before consuming the selected token", semanticBytes)
				}
				reads := len(fixture.buffer.reads)
				if semanticBytes, err = fixture.drain(test); err != errLegacyNativeNumberRejected || semanticBytes != 0 || len(fixture.buffer.reads) != reads {
					test.Fatal("repeated reject-only completion rebuilt diagnostics or reread source", semanticBytes, err)
				}
			})
		}
	}
}

func TestLegacyNativeNumberDiagnosticsAcceptedBitsEqual(test *testing.T) {
	for _, token := range []string{
		"0", "-0", "9007199254740993", "1.7976931348623157e308", "1.7976931348623158e308",
		"5e-324", "2.4703282292062327e-324", "-1e-9999",
		"-0e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17),
		"1." + strings.Repeat("0", 2*ConversationManifestChunkBytes+17) + "1",
	} {
		test.Run(fmt.Sprintf("bytes-%d/prefix-%.20s", len(token), token), func(test *testing.T) {
			var expected float64
			if err := json.Unmarshal([]byte(token), &expected); err != nil {
				test.Fatal("accepted numeric fixture is invalid", err)
			}
			for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
				fixture := legacyNativeNumberDiagnosticsStart(test, token, ConversationManifestChunkBytes-2, diagnostics)
				semanticBytes, err := fixture.drain(test)
				if err != nil || !fixture.number.complete || !fixture.number.converted || fixture.number.rejected ||
					fixture.number.errorText != nil || fixture.number.semantic != nil || semanticBytes != len(token) ||
					math.Float64bits(fixture.number.value) != math.Float64bits(expected) {
					test.Fatal("numeric diagnostics policy changed accepted value bits or materialized diagnostics", diagnostics, semanticBytes, err)
				}
			}
		})
	}
}

func legacyNativeNumberDiagnosticsRefuse(test *testing.T, fixture *legacyNativeNumberDiagnosticsFixture, diagnostics legacyNativeNumberDiagnostics, canceled bool) {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if canceled {
		cancel()
	}
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, workBytes: 73, loads: 1, numberDiagnostics: diagnostics}
	beforeNumber, beforeParser, beforeInput := *fixture.number, *fixture.number.parser, *input
	beforeReads := len(fixture.buffer.reads)
	var beforeDecimal legacyNativeFloat64Decimal
	if fixture.number.parser.decimal != nil {
		beforeDecimal = *fixture.number.parser.decimal
	}
	var beforeScalar legacyNativeScalar
	if fixture.number.errorText != nil {
		beforeScalar = *fixture.number.errorText
	}
	var beforeSemantic json.UnmarshalTypeError
	if fixture.number.semantic != nil {
		beforeSemantic = *fixture.number.semantic
	}
	var beforeText string
	var beforeCapacity int
	if fixture.number.errorText != nil && fixture.number.errorText.text != nil {
		beforeText, beforeCapacity = fixture.number.errorText.text.String(), fixture.number.errorText.text.Cap()
	}
	wanted := ErrIntegrity
	if canceled {
		wanted = context.Canceled
	}
	complete, err := fixture.number.step(ctx, input)
	afterInput := *input
	beforeInput.source.Read, afterInput.source.Read = nil, nil
	if complete || !errors.Is(err, wanted) || len(fixture.buffer.reads) != beforeReads ||
		!reflect.DeepEqual(*fixture.number, beforeNumber) || !reflect.DeepEqual(*fixture.number.parser, beforeParser) || !reflect.DeepEqual(afterInput, beforeInput) ||
		reflect.ValueOf(input.source.Read).Pointer() != reflect.ValueOf(fixture.source.Read).Pointer() {
		test.Fatal("refused numeric policy or canceled step mutated wrapper, reducer, input, or source reads", diagnostics, canceled, complete, err)
	}
	if fixture.number.parser.decimal != nil && !reflect.DeepEqual(*fixture.number.parser.decimal, beforeDecimal) {
		test.Fatal("refused numeric policy mutated aliased decimal state")
	}
	if fixture.number.errorText != nil && !reflect.DeepEqual(*fixture.number.errorText, beforeScalar) ||
		fixture.number.semantic != nil && !reflect.DeepEqual(*fixture.number.semantic, beforeSemantic) {
		test.Fatal("refused numeric policy mutated aliased diagnostic state")
	}
	if fixture.number.errorText != nil && fixture.number.errorText.text != nil &&
		(fixture.number.errorText.text.String() != beforeText || fixture.number.errorText.text.Cap() != beforeCapacity) {
		test.Fatal("refused numeric policy appended or reallocated raw diagnostics")
	}
}

func TestLegacyNativeNumberDiagnosticsInvalidAndChangedPolicyAtomic(test *testing.T) {
	for _, diagnostics := range []legacyNativeNumberDiagnostics{2, 255} {
		if number, err := newLegacyNativeNumberValue(0, 1, diagnostics); number != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("numeric constructor accepted an invalid diagnostics policy", diagnostics, err)
		}
	}
	for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
		for _, stage := range []string{"new", "partial", "diagnostic", "complete"} {
			if stage == "diagnostic" && diagnostics != legacyNativeNumberDiagnosticsFull {
				continue
			}
			test.Run(fmt.Sprintf("policy-%d/%s", diagnostics, stage), func(test *testing.T) {
				token := "1" + strings.Repeat("0", 2*ConversationManifestChunkBytes+17)
				fixture := legacyNativeNumberDiagnosticsStart(test, token, 17, diagnostics)
				if stage == "partial" {
					input := &legacyNormalizationInput{ctx: context.Background(), source: fixture.source, numberDiagnostics: diagnostics}
					complete, err := fixture.step(test, input.ctx, input)
					if err != nil || complete || fixture.number.parser.position <= 17 || fixture.number.parser.position >= 17+len(token) {
						test.Fatal("policy-switch fixture did not park a partially scanned number", err)
					}
				}
				if stage == "diagnostic" {
					for stepIndex := 0; stepIndex < 256 && fixture.number.errorText == nil; stepIndex++ {
						input := &legacyNormalizationInput{ctx: context.Background(), source: fixture.source}
						if complete, err := fixture.step(test, input.ctx, input); complete || err != nil {
							test.Fatal("full diagnostic fixture completed before exposing parked raw-text ownership", err)
						}
					}
					if fixture.number.errorText == nil || !fixture.number.converted || !fixture.number.rejected || fixture.number.complete {
						test.Fatal("full diagnostic fixture did not expose pending raw-text ownership")
					}
				}
				if stage == "complete" {
					if _, err := fixture.drain(test); err == nil || !fixture.number.complete {
						test.Fatal("policy-switch fixture did not reach terminal overflow", err)
					}
				}
				other := legacyNativeNumberDiagnosticsFull
				if diagnostics == other {
					other = legacyNativeNumberDiagnosticsRejectOnly
				}
				for _, changed := range []legacyNativeNumberDiagnostics{other, 2, 255} {
					legacyNativeNumberDiagnosticsRefuse(test, fixture, changed, false)
					legacyNativeNumberDiagnosticsRefuse(test, fixture, changed, true)
				}
				legacyNativeNumberDiagnosticsRefuse(test, fixture, diagnostics, true)
				if _, err := fixture.drain(test); err == nil || !fixture.number.complete {
					test.Fatal("refused policy switch damaged resumable numeric state", err)
				}
			})
		}
	}
}

func TestLegacyNativeNumberDiagnosticsRejectOnlySharedBudget(test *testing.T) {
	for _, loadsExhausted := range []bool{false, true} {
		test.Run(fmt.Sprintf("loads-exhausted-%t", loadsExhausted), func(test *testing.T) {
			fixture := legacyNativeNumberDiagnosticsStart(test, "1e309", 17, legacyNativeNumberDiagnosticsRejectOnly)
			input := &legacyNormalizationInput{ctx: context.Background(), source: fixture.source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
			if loadsExhausted {
				input.loads = 2
			} else {
				input.workBytes = ConversationManifestChunkBytes - 1
			}
			complete, err := fixture.step(test, input.ctx, input)
			if err != nil || complete || fixture.number.converted || fixture.number.parser.position != 17 || len(fixture.buffer.reads) != 0 {
				test.Fatal("reject-only numeric route reset an exhausted enclosing work/load budget", complete, err)
			}
			if _, err = fixture.drain(test); err != errLegacyNativeNumberRejected {
				test.Fatal("budget yield damaged resumable reject-only conversion", err)
			}
		})
	}
}

func TestLegacyNativeNumberDiagnosticsRejectOnlyDoesNotHideSuffixOrSourceFailure(test *testing.T) {
	token := "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)
	for _, fault := range []string{"syntax", "hash", "read", "canceled-read"} {
		test.Run(fault, func(test *testing.T) {
			selected := token
			if fault == "syntax" {
				selected += "x"
			}
			fixture := legacyNativeNumberDiagnosticsStart(test, selected, 17, legacyNativeNumberDiagnosticsRejectOnly)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			readSource := fixture.source.Read
			fixture.source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
				if offset >= 2*ConversationManifestChunkBytes && fault != "syntax" {
					if fault == "hash" {
						body, err := readSource(readContext, offset, length)
						body = append([]byte(nil), body...)
						body[0] = '8'
						return body, err
					}
					if fault == "canceled-read" {
						cancel()
					}
					return nil, io.ErrUnexpectedEOF
				}
				return readSource(readContext, offset, length)
			}
			for range 256 {
				input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, numberDiagnostics: legacyNativeNumberDiagnosticsRejectOnly}
				complete, err := fixture.step(test, ctx, input)
				if complete || err == errLegacyNativeNumberRejected || fixture.number.complete {
					test.Fatal("reject-only overflow bypassed the malformed or unauthenticated late suffix", fault, err)
				}
				if err == nil {
					continue
				}
				wanted := ErrPreparedDocRangeObject
				if fault == "syntax" {
					wanted = ErrIntegrity
				} else if fault == "canceled-read" {
					wanted = context.Canceled
				}
				if !errors.Is(err, wanted) {
					test.Fatal("reject-only numeric route changed suffix/source/context error priority", fault, err)
				}
				return
			}
			test.Fatal("late numeric failure did not terminate within its finite schedule")
		})
	}
}

func legacyNativeNumberDiagnosticsDecodeWire(test *testing.T, raw []byte, actual *wire, diagnostics legacyNativeNumberDiagnostics) error {
	test.Helper()
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[wire]())
	native, err := newLegacyNativeObject(reflect.ValueOf(actual), &metadata, 1, false)
	if err != nil {
		test.Fatal("numeric diagnostics wire constructor failed", err)
	}
	buffer := &legacySourceProofBuffer{body: raw}
	source := buffer.input()
	native.start(0, len(raw))
	var frame legacyJSONValueScanner
	for range 256 {
		ctx := context.Background()
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		if diagnostics != legacyNativeNumberDiagnosticsFull {
			input.numberDiagnostics = diagnostics
		}
		reads := len(buffer.reads)
		complete, err := native.step(ctx, input, &frame)
		if input.unbounded || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || len(buffer.reads)-reads > 2 {
			test.Fatal("numeric diagnostics wire route escaped shared work/source budgets")
		}
		if input.failure != nil {
			test.Fatal("numeric diagnostics wire fixture lost authenticated source access", input.failure)
		}
		if err != nil || complete {
			return err
		}
	}
	test.Fatal("numeric diagnostics wire exhausted its finite schedule")
	return ErrIntegrity
}

func TestLegacyNativeNumberDiagnosticsRejectedWirePreservesActualOldOutput(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	raw := []byte(`{"output":` + token + `}`)
	for _, old := range []any{"kept", float64(7), map[string]any{"kept": true}, []any{"kept"}} {
		test.Run(fmt.Sprintf("old-%T", old), func(test *testing.T) {
			expected := wire{Output: old}
			oracleErr := json.Unmarshal(raw, &expected)
			for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
				actual := wire{Output: old}
				err := legacyNativeNumberDiagnosticsDecodeWire(test, raw, &actual, diagnostics)
				if !reflect.DeepEqual(actual.Output, old) || !reflect.DeepEqual(actual, expected) {
					test.Fatal("rejected native number replaced the actual preexisting wire Output", diagnostics)
				}
				if diagnostics == legacyNativeNumberDiagnosticsFull {
					legacyNativeAnyReviewCompareTypeError(test, err, oracleErr)
				} else if err != errLegacyNativeNumberRejected {
					test.Fatal("reject-only wire did not preserve the private rejection sentinel", err)
				}
			}
		})
	}
}

func TestLegacyNativeNumberDiagnosticsDefaultGenericOverflowKeepsNilLeaves(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	for _, raw := range []string{
		`{"input":{"number":` + token + `,"tail":true}}`,
		`{"output":{"number":` + token + `,"tail":true}}`,
		`{"output":[` + token + `,{"number":` + token + `},null,-0]}`,
	} {
		var expected wire
		oracleErr := json.Unmarshal([]byte(raw), &expected)
		var actual wire
		err := legacyNativeNumberDiagnosticsDecodeWire(test, []byte(raw), &actual, legacyNativeNumberDiagnosticsFull)
		legacyNativeAnyReviewCompareTypeError(test, err, oracleErr)
		legacyNativeAnyNumberTree(test, "wire.input", actual.Input, expected.Input)
		legacyNativeAnyNumberTree(test, "wire.output", actual.Output, expected.Output)
		if !reflect.DeepEqual(actual, expected) {
			test.Fatal("default full diagnostics changed recoverable generic nil-leaf/container semantics")
		}
	}
}
