package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeTypedNumberUnitModel struct {
	Number *json.Number `json:"number"`
}

type legacyNativeTypedNumberUnitFixture struct {
	native *legacyNativeObject
	model  *legacyNativeTypedNumberUnitModel
	buffer *legacySourceProofBuffer
	source LegacyByteSource
}

func legacyNativeTypedNumberUnitStart(test *testing.T, token string, from int, prior *json.Number) *legacyNativeTypedNumberUnitFixture {
	test.Helper()
	model := &legacyNativeTypedNumberUnitModel{Number: prior}
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[legacyNativeTypedNumberUnitModel]())
	metadata.fields[0].typedNumber = true
	native, err := newLegacyNativeObject(reflect.ValueOf(model), &metadata, 1, false)
	if err != nil {
		test.Fatal("typed number fixture could not construct native object", err)
	}
	native.field, native.phase = 0, legacyNativeObjectApply
	native.valueStart, native.valueEnd = from, from+len(token)
	native.position, native.end, native.valueFirst = native.valueEnd, native.valueEnd, token[0]
	buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", from) + token)}
	return &legacyNativeTypedNumberUnitFixture{native: native, model: model, buffer: buffer, source: buffer.input()}
}

func (fixture *legacyNativeTypedNumberUnitFixture) step(test *testing.T, ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	test.Helper()
	work, loads, reads := input.workBytes, input.loads, len(fixture.buffer.reads)
	complete, err := fixture.native.decodeTypedNumber(ctx, input)
	if input.unbounded || input.workBytes < work || input.workBytes > ConversationManifestChunkBytes ||
		input.loads < loads || input.loads > 2 || len(fixture.buffer.reads)-reads > 2-loads {
		test.Fatal("typed number escaped the shared work or load budget", work, input.workBytes, loads, input.loads)
	}
	for _, read := range fixture.buffer.reads[reads:] {
		if read[0]%ConversationManifestChunkBytes != 0 || read[1] != min(ConversationManifestChunkBytes, fixture.source.Bytes-read[0]) {
			test.Fatal("typed number bypassed authenticated source parts", read)
		}
	}
	return complete, err
}

func (fixture *legacyNativeTypedNumberUnitFixture) drain(test *testing.T, diagnostics legacyNativeNumberDiagnostics, old string) error {
	test.Helper()
	for range 64 {
		ctx := context.Background()
		input := &legacyNormalizationInput{ctx: ctx, source: fixture.source, numberDiagnostics: diagnostics}
		complete, err := fixture.step(test, ctx, input)
		if input.failure != nil {
			return input.failure
		}
		if !complete && fixture.model.Number != nil && string(*fixture.model.Number) != old {
			test.Fatal("typed number changed its old pointee before successful validation")
		}
		if err != nil || complete {
			return err
		}
	}
	test.Fatal("typed number exhausted its finite unit schedule")
	return ErrIntegrity
}

func TestLegacyNativeTypedNumberUnitEligibility(test *testing.T) {
	for _, first := range []byte{'"', '-', '0', '9', 'n', 't', '[', '{'} {
		for _, length := range []int{LegacyCanonicalEncodingBytes, LegacyCanonicalEncodingBytes + 1} {
			native := &legacyNativeObject{valueEnd: length, valueFirst: first}
			want := length > LegacyCanonicalEncodingBytes && (first == '"' || first == '-' || first >= '0' && first <= '9')
			if native.typedNumberField(legacyNativeObjectField{typedNumber: true}) != want ||
				native.typedNumberField(legacyNativeObjectField{}) {
				test.Fatal("typed number eligibility changed its exact metadata, token kind, or size gate", first, length)
			}
		}
	}
}

func TestLegacyNativeTypedNumberUnitAcceptedLexicalText(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
		want  string
	}{
		{name: "raw", token: "-0." + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + "9E+000002", want: "-0." + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + "9E+000002"},
		{name: "escaped", token: `"\u0031` + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + `e\u002b0002"`, want: "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + "e+0002"},
	} {
		for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
			test.Run(fmt.Sprintf("%s/policy-%d", fixture.name, diagnostics), func(test *testing.T) {
				prior := json.Number("-7E+0003")
				unit := legacyNativeTypedNumberUnitStart(test, fixture.token, ConversationManifestChunkBytes-3, &prior)
				if err := unit.drain(test, diagnostics, string(prior)); err != nil || unit.model.Number != &prior ||
					string(prior) != fixture.want || unit.native.typedNumber != nil {
					test.Fatal("typed number lost lexical text, pointee identity, or completed ownership", err)
				}
				minimum, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(unit.model.Number))
				if err != nil || unit.native.heaps[0].err != nil || unit.native.heaps[0].bytes != minimum.bytes+unit.native.backingExcess[0] {
					test.Fatal("typed number transfer lost pointer, pointee, or spare builder capacity", minimum, unit.native.heaps[0], err)
				}
			})
		}
	}
}

func TestLegacyNativeTypedNumberUnitRejectedRawDiagnostic(test *testing.T) {
	padding := strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	for _, token := range []string{
		`"\u002b` + padding + `"`,
		`"1` + padding + `\u0000"`,
		`"1` + padding + "\ud55c\u200b🙂" + `\"\\\n"`,
		`"1` + padding + string([]byte{0xff, 0xc2}) + `"`,
	} {
		for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
			test.Run(fmt.Sprintf("bytes-%d/policy-%d/tail-%x", len(token), diagnostics, token[len(token)-8:]), func(test *testing.T) {
				unit := legacyNativeTypedNumberUnitStart(test, token, 7, nil)
				err := unit.drain(test, diagnostics, "")
				if unit.model.Number == nil || *unit.model.Number != "" || unit.native.typedNumber == nil || !unit.native.typedNumber.complete ||
					!unit.native.typedNumber.rejected || unit.native.typedNumber.scalar != nil {
					test.Fatal("invalid typed number lost native allocation or retained its decoded builder", err)
				}
				state := unit.native.typedNumber
				if diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
					if err != errLegacyNativeNumberRejected || state.errorText != nil || state.semantic != nil {
						test.Fatal("reject-only typed number constructed raw diagnostics", err)
					}
				} else {
					want := fmt.Sprintf("json: invalid number literal, trying to unmarshal %q into Number", []byte(token))
					if err == nil || err.Error() != want || state.errorText == nil || state.errorText.position != state.end || state.errorText.previous != nil {
						test.Fatal("full typed number diagnostic substituted decoded text or changed original raw quoting")
					}
				}
				reads := len(unit.buffer.reads)
				ctx := context.Background()
				input := &legacyNormalizationInput{ctx: ctx, source: unit.source, numberDiagnostics: diagnostics}
				complete, repeated := unit.step(test, ctx, input)
				if !complete || repeated != err || input.workBytes != 0 || len(unit.buffer.reads) != reads {
					test.Fatal("completed typed number rejection reread or rebuilt its diagnostic", complete, repeated)
				}
			})
		}
	}
}

func TestLegacyNativeTypedNumberUnitRetainedAndReservation(test *testing.T) {
	prior := json.Number("99")
	unit := legacyNativeTypedNumberUnitStart(test, "1"+strings.Repeat("0", 2*LegacyCanonicalEncodingBytes+17), 0, &prior)
	ctx := context.Background()
	blocked := &legacyNormalizationInput{ctx: ctx, source: unit.source, workBytes: ConversationManifestChunkBytes - 1, loads: 2}
	if complete, err := unit.step(test, ctx, blocked); complete || err != nil || unit.native.typedNumber != nil || len(unit.buffer.reads) != 0 || prior != "99" {
		test.Fatal("typed number allocated before reserving shared setup work", complete, err)
	}
	input := &legacyNormalizationInput{ctx: ctx, source: unit.source, workBytes: ConversationManifestChunkBytes - 4096}
	if complete, err := unit.step(test, ctx, input); complete || err != nil || prior != "99" || unit.native.typedNumber == nil {
		test.Fatal("typed number did not retain its old pointee during bounded copying", complete, err)
	}
	state := unit.native.typedNumber
	scalar := state.scalar
	if scalar == nil || scalar.text == nil || scalar.text.Cap() <= scalar.text.Len() {
		test.Fatal("typed number reservation fixture did not expose spare builder capacity")
	}
	var actual, expected docRangeHeaderHeapEstimate
	state.retained(&actual)
	expected.allocation(1, int64(unsafe.Sizeof(*state)))
	expected.allocation(1, int64(unsafe.Sizeof(*scalar)))
	expected.allocation(1, int64(unsafe.Sizeof(*scalar.text)))
	expected.allocation(int64(scalar.text.Cap()), 1)
	old, err := legacyNativeValueHeap(ctx, reflect.ValueOf(&prior))
	if err != nil || actual.err != nil || expected.err != nil || actual.bytes != expected.bytes || unit.native.heaps[0] != old {
		test.Fatal("typed number retained accounting lost actual builder capacity or old pointee storage", actual, expected, err)
	}
	if err := unit.drain(test, legacyNativeNumberDiagnosticsFull, "99"); err != nil || unit.model.Number != &prior {
		test.Fatal("reserved typed number did not finish its finite copy and validation", err)
	}
}

func TestLegacyNativeTypedNumberUnitOwnerPolicyAndCancellation(test *testing.T) {
	for _, mutation := range []string{"owner", "extent", "policy", "cancel"} {
		test.Run(mutation, func(test *testing.T) {
			prior := json.Number("17")
			unit := legacyNativeTypedNumberUnitStart(test, `"1`+strings.Repeat("0", 2*LegacyCanonicalEncodingBytes+17)+`"`, 0, &prior)
			ctx := context.Background()
			partial := &legacyNormalizationInput{ctx: ctx, source: unit.source, workBytes: ConversationManifestChunkBytes - 4096}
			if complete, err := unit.step(test, ctx, partial); complete || err != nil || unit.native.typedNumber == nil {
				test.Fatal("typed number fence fixture failed to pause", complete, err)
			}
			position, reads := unit.native.typedNumber.scalar.position, len(unit.buffer.reads)
			input := &legacyNormalizationInput{ctx: ctx, source: unit.source}
			want := ErrIntegrity
			switch mutation {
			case "owner":
				input.source.authority++
			case "extent":
				input.source.Bytes++
			case "policy":
				input.numberDiagnostics = legacyNativeNumberDiagnosticsRejectOnly
			case "cancel":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, input.ctx, want = canceled, canceled, context.Canceled
			}
			complete, err := unit.step(test, ctx, input)
			if complete || !errors.Is(err, want) || unit.native.typedNumber.scalar.position != position || input.workBytes != 0 ||
				len(unit.buffer.reads) != reads || unit.model.Number != &prior || prior != "17" {
				test.Fatal("typed number mixed a changed source, policy, or canceled prefix", complete, err)
			}
		})
	}
}

func TestLegacyNativeTypedNumberUnitDiagnosticReservesBeforeRead(test *testing.T) {
	prefix := legacyNativeTypedNumberErrorPrefix + `"\"x`
	padding := strings.Repeat("0", ConversationManifestChunkBytes-len(prefix))
	token := `"x` + padding + `"`
	prior := &strings.Builder{}
	prior.Grow(ConversationManifestChunkBytes)
	_, _ = prior.WriteString(prefix + padding)
	buffer := &legacySourceProofBuffer{body: []byte(token)}
	source := buffer.input()
	number := &legacyNativeTypedNumber{
		from: 0, end: len(token), rejected: true,
		errorText: &legacyNativeScalar{
			text: prior, position: len(token) - 1, end: len(token), expanded: true,
			maximum: 6*len(token) + len(legacyNativeTypedNumberErrorPrefix) + len(legacyNativeTypedNumberErrorSuffix) + 2,
		},
	}
	ctx := context.Background()
	partial := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: 1}
	complete, err := number.reject(ctx, partial)
	if complete || err != nil || number.errorText.text != prior || number.errorText.previous != nil || partial.workBytes != 1 || len(buffer.reads) != 0 {
		test.Fatal("typed number diagnostic read or allocated before its shared migration reservation", complete, err)
	}
	fresh := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err = number.reject(ctx, fresh)
	if complete || err != nil || number.errorText.previous != nil || number.errorText.text == prior ||
		fresh.workBytes != prior.Len() || number.errorText.text.String() != prior.String() || len(buffer.reads) != 0 {
		test.Fatal("typed number diagnostic did not dedicate a fresh quantum to its reserved backing copy", complete, err)
	}
	next := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err = number.reject(ctx, next)
	want := fmt.Sprintf("json: invalid number literal, trying to unmarshal %q into Number", []byte(token))
	if !complete || err == nil || err.Error() != want || next.workBytes > ConversationManifestChunkBytes || next.loads > 2 {
		test.Fatal("typed number diagnostic failed to resume exact raw quoting after its migration yield", complete, err)
	}
}
