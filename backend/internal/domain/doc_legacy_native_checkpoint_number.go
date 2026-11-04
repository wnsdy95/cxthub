package domain

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const legacyNativeCheckpointNumberMetadataBytes = 16384 + 6*4096

type legacyNativeCheckpointNumberDecimal struct {
	Digits     [800]byte                       `json:"digits"`
	Count      int                             `json:"count"`
	Point      int                             `json:"point"`
	Exponent   int                             `json:"exponent"`
	Shift      int                             `json:"shift"`
	ShiftDelta int                             `json:"shift_delta"`
	Bits       uint64                          `json:"bits"`
	Phase      legacyNativeFloat64DecimalPhase `json:"phase"`
	AfterShift legacyNativeFloat64DecimalPhase `json:"after_shift"`
	Negative   bool                            `json:"negative"`
	Truncated  bool                            `json:"truncated"`
	Rejected   bool                            `json:"rejected"`
}

type legacyNativeCheckpointNumberFloat struct {
	Digits         [800]byte                           `json:"digits"`
	Decimal        legacyNativeCheckpointNumberDecimal `json:"decimal"`
	HasDecimal     bool                                `json:"has_decimal"`
	Mantissa       uint64                              `json:"mantissa"`
	Bits           uint64                              `json:"bits"`
	SourceBound    bool                                `json:"source_bound"`
	From           int                                 `json:"from"`
	Position       int                                 `json:"position"`
	End            int                                 `json:"end"`
	Significant    int                                 `json:"significant"`
	MantissaDigits int                                 `json:"mantissa_digits"`
	RetainedDigits int                                 `json:"retained_digits"`
	PrimaryPoint   int                                 `json:"primary_point"`
	DecimalPoint   int                                 `json:"decimal_point"`
	Exponent       int                                 `json:"exponent"`
	Phase          uint8                               `json:"phase"`
	Negative       bool                                `json:"negative"`
	NegativeExp    bool                                `json:"negative_exp"`
	Dot            bool                                `json:"dot"`
	PrimaryTrunc   bool                                `json:"primary_trunc"`
	DecimalTrunc   bool                                `json:"decimal_trunc"`
	Rejected       bool                                `json:"rejected"`
}

type legacyNativeCheckpointNumberSemantic struct {
	Present bool   `json:"present"`
	Type    string `json:"type"`
	Offset  int64  `json:"offset"`
	Struct  string `json:"struct"`
	Field   string `json:"field"`
}

type legacyNativeCheckpointNumberState struct {
	Version      uint8                                `json:"version"`
	Binding      ContentHash                          `json:"binding,omitempty"`
	SourceBytes  int                                  `json:"source_bytes"`
	Parser       legacyNativeCheckpointNumberFloat    `json:"parser"`
	ErrorText    legacyNativeCheckpointScalar         `json:"error_text"`
	HasErrorText bool                                 `json:"has_error_text"`
	Semantic     legacyNativeCheckpointNumberSemantic `json:"semantic"`
	ValueBits    uint64                               `json:"value_bits"`
	Converted    bool                                 `json:"converted"`
	Rejected     bool                                 `json:"rejected"`
	Complete     bool                                 `json:"complete"`
	Diagnostics  legacyNativeNumberDiagnostics        `json:"diagnostics"`
	PartBytes    int                                  `json:"part_bytes"`
	SourceParts  legacyNativeCheckpointTree           `json:"source_parts"`
}

type legacyNativeCheckpointNumberSourcePart struct {
	Ordinal int         `json:"ordinal"`
	Hash    ContentHash `json:"hash"`
	Bytes   int         `json:"bytes"`
}

func (state legacyNativeCheckpointNumberState) needsSource() bool {
	return state.Diagnostics == legacyNativeNumberDiagnosticsFull && !state.Complete && (!state.Converted || state.Rejected)
}

func legacyNativeCheckpointNumberSourceRange(from, position, sourceBytes, partBytes int) (int, int) {
	if from >= position {
		return 0, 0
	}
	first, limit := from/partBytes, position/partBytes
	if position == sourceBytes && position%partBytes != 0 {
		limit++
	}
	if limit <= first {
		return 0, 0
	}
	return first, limit
}

func (state legacyNativeCheckpointNumberState) sourceRange() (int, int) {
	if !state.needsSource() || state.PartBytes == 0 {
		return 0, 0
	}
	from := state.Parser.From
	if state.HasErrorText {
		from = state.ErrorText.State.Position
	}
	return legacyNativeCheckpointNumberSourceRange(from, state.Parser.Position, state.SourceBytes, state.PartBytes)
}

func (state legacyNativeCheckpointNumberState) validateSource() error {
	if !state.needsSource() {
		if state.PartBytes != 0 || state.SourceParts != (legacyNativeCheckpointTree{}) {
			return ErrIntegrity
		}
		return nil
	}
	if state.PartBytes != LegacyNormalizationPartBytes && state.PartBytes != ConversationManifestChunkBytes || !state.SourceParts.valid() {
		return ErrIntegrity
	}
	first, limit := state.sourceRange()
	if state.SourceParts.Count != uint64(limit-first) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointNumberLiveGeometry(live LegacyByteSource, sourceBytes int) error {
	partBytes := live.partBytes()
	if live.Bytes != sourceBytes || live.Bytes <= 0 || live.Bytes > MaxFinalizedDocBytes || partBytes == 0 ||
		live.Read == nil || len(live.Parts) != 1+(live.Bytes-1)/partBytes {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointNumberLivePart(live LegacyByteSource, ordinal int) error {
	if ordinal < 0 || ordinal >= len(live.Parts) || live.partBytes() == 0 ||
		len(live.Parts[ordinal].Hash) != len(contentHashPrefix)+64 ||
		ValidateContentHash(live.Parts[ordinal].Hash) != nil ||
		live.Parts[ordinal].Bytes != int64(min(live.partBytes(), live.Bytes-ordinal*live.partBytes())) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointNumberCapture(number *legacyNativeNumberValue, sourceBytes int) (legacyNativeCheckpointNumberState, error) {
	if number == nil || number.parser == nil {
		return legacyNativeCheckpointNumberState{}, ErrIntegrity
	}
	parser := number.parser
	state := legacyNativeCheckpointNumberState{
		Version: legacyNativeCheckpointVersion, SourceBytes: sourceBytes,
		Parser: legacyNativeCheckpointNumberFloat{
			Digits: parser.digits, HasDecimal: parser.decimal != nil,
			Mantissa: parser.mantissa, Bits: parser.bits, SourceBound: parser.authority != 0,
			From: parser.from, Position: parser.position, End: parser.end,
			Significant: parser.significant, MantissaDigits: parser.mantissaDigits, RetainedDigits: parser.retainedDigits,
			PrimaryPoint: parser.primaryPoint, DecimalPoint: parser.decimalPoint, Exponent: parser.exponent,
			Phase: parser.phase, Negative: parser.negative, NegativeExp: parser.negativeExp, Dot: parser.dot,
			PrimaryTrunc: parser.primaryTrunc, DecimalTrunc: parser.decimalTrunc, Rejected: parser.rejected,
		},
		HasErrorText: number.errorText != nil, ValueBits: math.Float64bits(number.value),
		Converted: number.converted, Rejected: number.rejected, Complete: number.complete, Diagnostics: number.diagnostics,
	}
	if parser.authority == 0 && parser.sourceBytes != 0 || parser.authority != 0 && parser.sourceBytes != sourceBytes {
		return legacyNativeCheckpointNumberState{}, ErrIntegrity
	}
	if parser.decimal != nil {
		decimal := parser.decimal
		state.Parser.Decimal = legacyNativeCheckpointNumberDecimal{
			Digits: decimal.digits, Count: decimal.count, Point: decimal.point, Exponent: decimal.exponent,
			Shift: decimal.shift, ShiftDelta: decimal.shiftDelta, Bits: decimal.bits,
			Phase: decimal.phase, AfterShift: decimal.afterShift, Negative: decimal.negative,
			Truncated: decimal.truncated, Rejected: decimal.rejected,
		}
	}
	if number.semantic != nil {
		semantic := number.semantic
		if semantic.Type != reflect.TypeFor[float64]() || number.errorText == nil || number.errorText.text == nil ||
			len(semantic.Value) != number.errorText.text.Len() ||
			unsafe.StringData(semantic.Value) != unsafe.StringData(number.errorText.text.String()) {
			return legacyNativeCheckpointNumberState{}, ErrIntegrity
		}
		state.Semantic = legacyNativeCheckpointNumberSemantic{
			Present: true, Type: "float64", Offset: semantic.Offset, Struct: semantic.Struct, Field: semantic.Field,
		}
	}
	return state, nil
}

func (state legacyNativeCheckpointNumberState) validate() error {
	if err := state.validateControl(); err != nil {
		return err
	}
	if err := state.validateErrorText(true); err != nil {
		return err
	}
	return state.validateSource()
}

func (state legacyNativeCheckpointNumberState) validateControl() error {
	if state.Binding != "" && (len(state.Binding) != len(contentHashPrefix)+64 || ValidateContentHash(state.Binding) != nil) {
		return ErrIntegrity
	}
	if state.Version != legacyNativeCheckpointVersion || state.SourceBytes <= 0 || state.SourceBytes > MaxFinalizedDocBytes ||
		state.Diagnostics != legacyNativeNumberDiagnosticsFull && state.Diagnostics != legacyNativeNumberDiagnosticsRejectOnly {
		return ErrIntegrity
	}
	if err := state.Parser.validate(state.SourceBytes); err != nil {
		return err
	}
	if state.Converted != (state.Parser.Phase == legacyNativeFloat64Done) || state.Rejected != state.Parser.Rejected ||
		state.Complete && !state.Converted || !state.Converted && state.ValueBits != 0 ||
		state.Converted && state.ValueBits != state.Parser.Bits ||
		state.Converted && !state.Rejected && !state.Complete {
		return ErrIntegrity
	}
	if state.Diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
		if state.HasErrorText || state.Semantic.Present || state.Complete != state.Converted {
			return ErrIntegrity
		}
	} else if state.HasErrorText && (!state.Converted || !state.Rejected) ||
		state.Semantic.Present != (state.Rejected && state.Complete) {
		return ErrIntegrity
	}
	semantic := state.Semantic
	if semantic.Present {
		if !state.HasErrorText || semantic.Type != "float64" || semantic.Offset <= 0 ||
			semantic.Offset > int64(MaxFinalizedDocBytes)+1 || len(semantic.Struct) > 4096 ||
			len(semantic.Field) > 4096-len(semantic.Struct) || !utf8.ValidString(semantic.Struct) || !utf8.ValidString(semantic.Field) {
			return ErrIntegrity
		}
	} else if semantic != (legacyNativeCheckpointNumberSemantic{}) {
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeCheckpointNumberState) validateErrorText(checkTree bool) error {
	if !state.HasErrorText {
		if state.ErrorText != (legacyNativeCheckpointScalar{}) {
			return ErrIntegrity
		}
		return nil
	}
	scalar := state.ErrorText.State
	if err := scalar.validate(state.SourceBytes); err != nil {
		return err
	}
	if !scalar.Present || scalar.Expanded || scalar.Position < state.Parser.From || scalar.End != state.Parser.End ||
		scalar.Maximum != state.Parser.End-state.Parser.From+len("number ") ||
		scalar.Bytes != scalar.Position-state.Parser.From+len("number ") || state.ErrorText.Text.Bytes != scalar.Bytes ||
		checkTree && (!state.ErrorText.Text.Tree.valid() || state.ErrorText.Text.Tree.Count != uint64(scalar.Bytes)) ||
		state.Complete && scalar.Position != scalar.End {
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeCheckpointNumberFloat) validate(sourceBytes int) error {
	if state.From < 0 || state.End <= state.From || state.End > sourceBytes || state.Position < state.From || state.Position > state.End ||
		state.End-state.From > int(^uint(0)>>1)-(99999+19) || state.Phase > legacyNativeFloat64Done {
		return ErrIntegrity
	}
	consumed := state.Position - state.From
	if state.SourceBound != (consumed > 0) || state.Significant < 0 || state.Significant > consumed ||
		state.MantissaDigits != min(state.Significant, 19) || state.RetainedDigits != min(state.Significant, len(state.Digits)) ||
		state.Exponent < 0 || state.Exponent > 99999 ||
		state.PrimaryPoint < -consumed-99999 || state.PrimaryPoint > consumed+99999 ||
		state.DecimalPoint < -consumed-99999 || state.DecimalPoint > consumed+99999 {
		return ErrIntegrity
	}
	var mantissa uint64
	primaryTrunc := state.DecimalTrunc
	for index, digit := range state.Digits {
		if index < state.RetainedDigits {
			if digit < '0' || digit > '9' || index == 0 && digit == '0' {
				return ErrIntegrity
			}
			if index < state.MantissaDigits {
				mantissa = mantissa*10 + uint64(digit-'0')
			} else if digit != '0' {
				primaryTrunc = true
			}
		} else if digit != 0 {
			return ErrIntegrity
		}
	}
	if state.Mantissa != mantissa || state.PrimaryTrunc != primaryTrunc || state.DecimalTrunc && state.Significant <= len(state.Digits) {
		return ErrIntegrity
	}
	if state.Phase >= legacyNativeFloat64Convert {
		if state.Position != state.End {
			return ErrIntegrity
		}
	} else if state.Position == state.End || state.HasDecimal || state.Bits != 0 || state.Rejected {
		return ErrIntegrity
	}
	if state.Phase != legacyNativeFloat64Done && (state.Bits != 0 || state.Rejected) ||
		state.Phase == legacyNativeFloat64Done && !legacyNativeCheckpointNumberBits(state.Bits, state.Negative, state.Rejected) {
		return ErrIntegrity
	}
	primaryPoint, decimalPoint := state.PrimaryPoint, state.DecimalPoint
	if state.Phase >= legacyNativeFloat64Convert {
		exponent := state.Exponent
		if state.NegativeExp {
			exponent = -exponent
		}
		primaryPoint -= exponent
		decimalPoint -= exponent
		if !state.Dot && (primaryPoint != state.Significant || decimalPoint != state.RetainedDigits) {
			return ErrIntegrity
		}
	} else if !state.Dot && (primaryPoint != decimalPoint || primaryPoint > 0 || state.Significant > 0 && primaryPoint != 0) {
		return ErrIntegrity
	}
	if state.Dot && (primaryPoint < -consumed || primaryPoint > state.Significant || decimalPoint != min(primaryPoint, len(state.Digits))) {
		return ErrIntegrity
	}
	if state.Phase < legacyNativeFloat64ExponentFirst && (state.Exponent != 0 || state.NegativeExp) ||
		state.Phase < legacyNativeFloat64FractionFirst && state.Dot {
		return ErrIntegrity
	}
	signBytes := 0
	if state.Negative {
		signBytes = 1
	}
	switch state.Phase {
	case legacyNativeFloat64Start:
		if consumed != 0 || state.Negative || state.Significant != 0 || primaryPoint != 0 || decimalPoint != 0 {
			return ErrIntegrity
		}
	case legacyNativeFloat64IntegerFirst:
		if consumed != 1 || !state.Negative || state.Significant != 0 || primaryPoint != 0 || decimalPoint != 0 {
			return ErrIntegrity
		}
	case legacyNativeFloat64IntegerZero:
		if consumed != 1+signBytes || state.Significant != 0 || primaryPoint != -1 || decimalPoint != -1 {
			return ErrIntegrity
		}
	case legacyNativeFloat64Integer:
		if state.Significant == 0 || consumed != state.Significant+signBytes {
			return ErrIntegrity
		}
	case legacyNativeFloat64FractionFirst, legacyNativeFloat64Fraction:
		if !state.Dot || consumed < 2+signBytes || state.Phase == legacyNativeFloat64Fraction && consumed < 3+signBytes {
			return ErrIntegrity
		}
	case legacyNativeFloat64ExponentFirst, legacyNativeFloat64ExponentDigitFirst:
		if state.Exponent != 0 || consumed < 2+signBytes || state.Phase == legacyNativeFloat64ExponentFirst && state.NegativeExp ||
			state.Phase == legacyNativeFloat64ExponentDigitFirst && consumed < 3+signBytes {
			return ErrIntegrity
		}
	case legacyNativeFloat64Exponent:
		if consumed < 3+signBytes {
			return ErrIntegrity
		}
	}
	if !state.HasDecimal {
		if state.Decimal != (legacyNativeCheckpointNumberDecimal{}) || state.Rejected {
			return ErrIntegrity
		}
		if state.Phase == legacyNativeFloat64Done {
			exponent := 0
			if state.Mantissa != 0 {
				exponent = state.PrimaryPoint - state.MantissaDigits
			}
			bits, valid := legacyNativeFloat64Fast(state.Mantissa, exponent, state.Negative, state.PrimaryTrunc)
			if !valid || bits != state.Bits {
				return ErrIntegrity
			}
		}
		return nil
	}
	if state.Phase < legacyNativeFloat64Convert || state.Decimal.Negative != state.Negative {
		return ErrIntegrity
	}
	if err := state.Decimal.validate(state); err != nil {
		return err
	}
	if state.Phase == legacyNativeFloat64Done {
		if state.Decimal.Phase != legacyNativeFloat64DecimalDone || state.Bits != state.Decimal.Bits || state.Rejected != state.Decimal.Rejected {
			return ErrIntegrity
		}
	} else if state.Decimal.Phase == legacyNativeFloat64DecimalDone {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointNumberBits(bits uint64, negative, rejected bool) bool {
	if rejected {
		return bits == 0
	}
	return bits>>52&2047 != 2047 && (bits>>63 != 0) == negative
}

func (state legacyNativeCheckpointNumberDecimal) validate(parser legacyNativeCheckpointNumberFloat) error {
	if state.Count < 0 || state.Count > len(state.Digits) || state.Phase > legacyNativeFloat64DecimalDone ||
		state.Point < -parser.End-100000 || state.Point > parser.End+100000 ||
		state.Exponent < -4096 || state.Exponent > 4096 || state.Shift < -4096 || state.Shift > 53 ||
		state.ShiftDelta < 0 || state.ShiftDelta > 4096 ||
		state.AfterShift != legacyNativeFloat64DecimalStart && state.AfterShift != legacyNativeFloat64DecimalRange && state.AfterShift != legacyNativeFloat64DecimalRound {
		return ErrIntegrity
	}
	for index, digit := range state.Digits {
		if index < state.Count {
			if digit < '0' || digit > '9' || index == 0 && digit == '0' {
				return ErrIntegrity
			}
		} else if digit != 0 && (digit < '0' || digit > '9') {
			return ErrIntegrity
		}
	}
	if state.Phase != legacyNativeFloat64DecimalDone && (state.Bits != 0 || state.Rejected) ||
		state.Phase == legacyNativeFloat64DecimalDone && !legacyNativeCheckpointNumberBits(state.Bits, state.Negative, state.Rejected) ||
		parser.DecimalTrunc && !state.Truncated {
		return ErrIntegrity
	}
	if state.Phase == legacyNativeFloat64DecimalStart {
		if state.Digits != parser.Digits || state.Count != parser.RetainedDigits || state.Point != parser.DecimalPoint ||
			state.Truncated != parser.DecimalTrunc || state.Exponent != 0 {
			return ErrIntegrity
		}
	} else if state.Phase != legacyNativeFloat64DecimalDone && (state.Point < -340 || state.Point > 320) {
		return ErrIntegrity
	}
	if state.Phase < legacyNativeFloat64DecimalRange && state.Phase != legacyNativeFloat64DecimalShift &&
		(state.Shift != 0 || state.ShiftDelta != 0 || state.AfterShift != legacyNativeFloat64DecimalStart) {
		return ErrIntegrity
	}
	if state.Phase == legacyNativeFloat64DecimalShift {
		switch state.AfterShift {
		case legacyNativeFloat64DecimalRange:
			if state.Exponent >= -1022 || state.Shift >= 0 || state.ShiftDelta != -state.Exponent-1022 || state.Shift < -state.ShiftDelta {
				return ErrIntegrity
			}
		case legacyNativeFloat64DecimalRound:
			if state.Shift <= 0 || state.ShiftDelta != 0 || state.Exponent < -1022 || state.Exponent >= 1024 {
				return ErrIntegrity
			}
		default:
			return ErrIntegrity
		}
	} else if state.Shift != 0 || state.ShiftDelta != 0 {
		return ErrIntegrity
	}
	if state.Phase == legacyNativeFloat64DecimalRound &&
		(state.AfterShift != legacyNativeFloat64DecimalRound || state.Point < -340 || state.Point > 20 || state.Exponent < -1022 || state.Exponent >= 1024) {
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeCheckpointNumberState) native(authority uint64) *legacyNativeNumberValue {
	saved := state.Parser
	parser := &legacyNativeFloat64{
		digits: saved.Digits, mantissa: saved.Mantissa, bits: saved.Bits,
		from: saved.From, position: saved.Position, end: saved.End,
		significant: saved.Significant, mantissaDigits: saved.MantissaDigits, retainedDigits: saved.RetainedDigits,
		primaryPoint: saved.PrimaryPoint, decimalPoint: saved.DecimalPoint, exponent: saved.Exponent,
		phase: saved.Phase, negative: saved.Negative, negativeExp: saved.NegativeExp, dot: saved.Dot,
		primaryTrunc: saved.PrimaryTrunc, decimalTrunc: saved.DecimalTrunc, rejected: saved.Rejected,
	}
	if saved.SourceBound {
		parser.authority, parser.sourceBytes = authority, state.SourceBytes
	}
	if saved.HasDecimal {
		decimal := saved.Decimal
		parser.decimal = &legacyNativeFloat64Decimal{
			digits: decimal.Digits, count: decimal.Count, point: decimal.Point, exponent: decimal.Exponent,
			shift: decimal.Shift, shiftDelta: decimal.ShiftDelta, bits: decimal.Bits, phase: decimal.Phase,
			afterShift: decimal.AfterShift, negative: decimal.Negative, truncated: decimal.Truncated, rejected: decimal.Rejected,
		}
	}
	return &legacyNativeNumberValue{
		parser: parser, value: math.Float64frombits(state.ValueBits), converted: state.Converted,
		rejected: state.Rejected, complete: state.Complete, diagnostics: state.Diagnostics,
	}
}

type legacyNativeCheckpointNumberWriter struct {
	state          legacyNativeCheckpointNumberState
	io             legacyNativeCheckpointIO
	scalar         *legacyNativeCheckpointScalarWriter
	ref            legacyNativeCheckpointRef
	budget         int64
	ready          bool
	closed         bool
	failure        error
	live           LegacyByteSource
	parts          legacyNativeCheckpointListWriter[legacyNativeCheckpointNumberSourcePart]
	authority      uint64
	diagnosticFrom int
	sourceNext     int
	sourceBound    bool
	sourceDone     bool
	started        bool
}

func newLegacyNativeCheckpointNumberWriter(ctx context.Context, number *legacyNativeNumberValue, storage legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointNumberWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if heapBudget <= 0 || storage.Put == nil {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointNumberWriter{})))
	if planned.err != nil || planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	state, err := legacyNativeCheckpointNumberCapture(number, sourceBytes)
	if err != nil {
		return nil, err
	}
	if state.HasErrorText {
		scalar := number.errorText
		if scalar.text == nil {
			return nil, ErrIntegrity
		}
		accepted := scalar.text
		if scalar.previous != nil {
			accepted = scalar.previous
		}
		if accepted == nil || accepted.Len() < len("number ") || accepted.String()[:len("number ")] != "number " ||
			state.Semantic.Present && (scalar.previous != nil || scalar.buffered != 0 || scalar.position != scalar.end) {
			return nil, ErrIntegrity
		}
		state.ErrorText.State = legacyNativeScalarCheckpoint{
			Version: 1, Position: scalar.position, End: scalar.end, Maximum: scalar.maximum,
			Bytes: accepted.Len(), Capacity: scalar.text.Cap(), Present: true, Expanded: scalar.expanded,
		}
		state.ErrorText.Text.Bytes = accepted.Len()
	}
	if err := state.validateControl(); err != nil {
		return nil, err
	}
	if err := state.validateErrorText(false); err != nil {
		return nil, err
	}
	planned.allocation(int64(len(state.Semantic.Struct)), 1)
	planned.allocation(int64(len(state.Semantic.Field)), 1)
	if state.HasErrorText {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointScalarWriter{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeScalarSnapshot{})))
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTextWriter{})))
	}
	if planned.err != nil || planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	state.Semantic.Struct, state.Semantic.Field = strings.Clone(state.Semantic.Struct), strings.Clone(state.Semantic.Field)
	diagnosticFrom := state.Parser.From
	if state.HasErrorText {
		diagnosticFrom = state.ErrorText.State.Position
	}
	work := &legacyNativeCheckpointNumberWriter{
		state: state, io: storage, budget: heapBudget, authority: number.parser.authority, diagnosticFrom: diagnosticFrom,
	}
	if state.HasErrorText {
		work.scalar, err = newLegacyNativeCheckpointScalarWriter(ctx, number.errorText, storage)
		if err != nil {
			work.close()
			return nil, err
		}
		work.state.ErrorText = legacyNativeCheckpointScalar{}
	}
	if err := work.checkBudget(0); err != nil {
		work.close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointNumberWriter) bindSource(ctx context.Context, live LegacyByteSource) error {
	if work == nil {
		return ErrIntegrity
	}
	if work.failure != nil {
		return work.failure
	}
	refuse := func(err error) error {
		_, failure := work.fail(err)
		return failure
	}
	if work.closed || work.started || work.sourceBound {
		return refuse(ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return refuse(err)
	}
	if err := legacyNativeCheckpointNumberLiveGeometry(live, work.state.SourceBytes); err != nil {
		return refuse(err)
	}
	if live.authority == 0 || work.state.Parser.SourceBound && live.authority != work.authority {
		return refuse(ErrIntegrity)
	}
	if work.state.needsSource() {
		work.state.PartBytes = live.partBytes()
	}
	live.Read, live.authority = nil, 0
	work.live, work.sourceBound, work.authority = live, true, 0
	if err := work.checkBudget(0); err != nil {
		return refuse(err)
	}
	return nil
}

func (work *legacyNativeCheckpointNumberWriter) captureSource(ctx context.Context) (bool, error) {
	if err := work.checkBudget(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	first, limit := 0, 0
	if work.state.needsSource() {
		first, limit = legacyNativeCheckpointNumberSourceRange(work.diagnosticFrom, work.state.Parser.Position, work.state.SourceBytes, work.state.PartBytes)
	}
	for actions := 0; actions < legacyNativeCheckpointPageItems && work.sourceNext < len(work.live.Parts); actions++ {
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		ordinal := work.sourceNext
		if err := legacyNativeCheckpointNumberLivePart(work.live, ordinal); err != nil {
			return work.fail(err)
		}
		if ordinal >= first && ordinal < limit {
			part := work.live.Parts[ordinal]
			if err := work.parts.append(ctx, work.io, legacyNativeCheckpointNumberSourcePart{
				Ordinal: ordinal, Hash: ContentHash(strings.Clone(string(part.Hash))), Bytes: int(part.Bytes),
			}); err != nil {
				return work.fail(err)
			}
		}
		work.sourceNext++
	}
	if work.sourceNext == len(work.live.Parts) {
		if work.state.needsSource() {
			tree, err := work.parts.finish(ctx, work.io)
			if err != nil {
				return work.fail(err)
			}
			work.state.SourceParts = tree
		}
		work.parts.close()
		work.live = LegacyByteSource{}
		work.sourceDone = true
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	return false, nil
}

func (work *legacyNativeCheckpointNumberWriter) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	work.started = true
	if work.state.needsSource() && !work.sourceBound {
		return work.fail(ErrIntegrity)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	if work.scalar != nil {
		if err := work.checkBudget(4 * LegacyCanonicalEncodingBytes); err != nil {
			return work.fail(err)
		}
		ready, err := work.scalar.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.state.ErrorText, err = work.scalar.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.scalar.close()
			work.scalar = nil
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.sourceBound && !work.sourceDone {
		return work.captureSource(ctx)
	}
	if err := work.state.validate(); err != nil {
		return work.fail(err)
	}
	maximum := 16384 + 6*(len(work.state.Semantic.Struct)+len(work.state.Semantic.Field))
	if err := work.checkBudget(4 * maximum); err != nil {
		return work.fail(err)
	}
	ref, err := legacyNativeCheckpointWriteJSON(ctx, work.io, work.state)
	if err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if !ref.valid() || ref.Bytes > maximum {
		return work.fail(ErrIntegrity)
	}
	work.ref, work.ready = ref, true
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	return true, nil
}

func (work *legacyNativeCheckpointNumberWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Binding)), 1)
	heap.include(work.live.checkpointReaderHeap)
	heap.allocation(int64(len(work.state.Semantic.Struct)), 1)
	heap.allocation(int64(len(work.state.Semantic.Field)), 1)
	heap.allocation(int64(len(work.state.ErrorText.Text.Tree.Ref.Hash)), 1)
	heap.allocation(int64(len(work.ref.Hash)), 1)
	heap.allocation(int64(len(work.state.SourceParts.Ref.Hash)), 1)
	heap.allocation(int64(len(work.live.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	heap.allocation(int64(len(work.live.Parts)), int64(len(contentHashPrefix)+64+32))
	parts, partsErr := work.parts.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: parts, err: partsErr})
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointNumberWriter) checkBudget(scratch int) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.allocation(int64(scratch), 1)
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointNumberWriter) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointRef{}, failure
	}
	if !work.ready || !work.ref.valid() {
		_, err := work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, err
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeCheckpointNumberWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointNumberWriter) close() {
	if work != nil {
		if work.scalar != nil {
			work.scalar.close()
		}
		work.parts.close()
		failure := work.failure
		*work = legacyNativeCheckpointNumberWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointNumberRestore struct {
	state       legacyNativeCheckpointNumberState
	io          legacyNativeCheckpointIO
	ref         legacyNativeCheckpointRef
	scalar      *legacyNativeCheckpointScalarRestore
	number      *legacyNativeNumberValue
	authority   uint64
	sourceBytes int
	budget      int64
	loaded      bool
	ready       bool
	closed      bool
	failure     error
	live        LegacyByteSource
	parts       []ConversationManifestChunk
	partCopied  int
	sourceParts *legacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart]
	sourceNext  int
	sourceDone  bool
}

func newLegacyNativeCheckpointNumberRestore(ctx context.Context, ref legacyNativeCheckpointRef, storage legacyNativeCheckpointIO, currentLiveSource LegacyByteSource, heapBudget int64) (*legacyNativeCheckpointNumberRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > legacyNativeCheckpointNumberMetadataBytes ||
		storage.Read == nil || currentLiveSource.Bytes <= 0 || currentLiveSource.Bytes > MaxFinalizedDocBytes || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeCheckpointNumberLiveGeometry(currentLiveSource, currentLiveSource.Bytes); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointNumberRestore{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	planned.allocation(int64(4*ref.Bytes), 1)
	planned.allocation(int64(len(currentLiveSource.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	planned.allocation(int64(len(currentLiveSource.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	planned.allocation(int64(2*len(currentLiveSource.Parts)), int64(len(contentHashPrefix)+64+32))
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	return &legacyNativeCheckpointNumberRestore{
		io: storage, ref: ref, authority: currentLiveSource.authority, sourceBytes: currentLiveSource.Bytes, budget: heapBudget,
		live: currentLiveSource, parts: make([]ConversationManifestChunk, len(currentLiveSource.Parts)),
	}, nil
}

func (work *legacyNativeCheckpointNumberRestore) restoreSource(ctx context.Context) (bool, error) {
	if work.parts != nil {
		for actions := 0; actions < legacyNativeCheckpointPageItems && work.partCopied < len(work.parts); actions++ {
			if err := ctx.Err(); err != nil {
				return work.fail(err)
			}
			ordinal := work.partCopied
			if err := legacyNativeCheckpointNumberLivePart(work.live, ordinal); err != nil {
				return work.fail(err)
			}
			part := work.live.Parts[ordinal]
			part.Hash = ContentHash(strings.Clone(string(part.Hash)))
			work.parts[ordinal] = part
			work.partCopied++
		}
		if work.partCopied == len(work.parts) {
			work.live.Parts, work.parts = work.parts, nil
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	first, limit := work.state.sourceRange()
	if first == limit {
		work.sourceDone = true
		return false, nil
	}
	if err := work.checkBudget(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if work.sourceParts == nil {
		reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart](work.state.SourceParts, work.io)
		if err != nil {
			return work.fail(err)
		}
		work.sourceParts, work.sourceNext = reader, first
	}
	part, present, err := work.sourceParts.next(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !present {
		if work.sourceNext != limit {
			return work.fail(ErrIntegrity)
		}
		work.sourceParts.close()
		work.sourceParts = nil
		work.sourceDone = true
	} else {
		if work.sourceNext >= limit || part.Ordinal != work.sourceNext || len(part.Hash) != len(contentHashPrefix)+64 || ValidateContentHash(part.Hash) != nil ||
			part.Bytes <= 0 || part.Bytes > work.live.partBytes() ||
			work.live.Parts[part.Ordinal].Hash != part.Hash || work.live.Parts[part.Ordinal].Bytes != int64(part.Bytes) ||
			part.Ordinal*work.live.partBytes()+part.Bytes > work.state.Parser.Position {
			return work.fail(ErrIntegrity)
		}
		work.sourceNext++
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	return false, nil
}

func legacyNativeCheckpointNumberReaderHeap() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(4, int64(unsafe.Sizeof(LegacyByteSource{})))
	heap.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointIO{})))
	heap.allocation(1, 256)
	return heap
}

func (work *legacyNativeCheckpointNumberRestore) source() LegacyByteSource {
	if work == nil || work.closed || work.failure != nil || !work.ready || !work.sourceDone {
		return LegacyByteSource{}
	}
	live := work.live
	first, limit := work.state.sourceRange()
	if first == limit {
		return live
	}
	store := work.io
	restored := live
	restored.checkpointReaderHeap.include(legacyNativeCheckpointNumberReaderHeap())
	restored.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		partBytes := live.partBytes()
		if offset >= 0 && offset < live.Bytes && offset%partBytes == 0 {
			ordinal := offset / partBytes
			if ordinal >= first && ordinal < limit {
				part := live.Parts[ordinal]
				if length != int(part.Bytes) {
					return nil, ErrIntegrity
				}
				dependency := live
				dependency.Read = func(current context.Context, readOffset, readLength int) ([]byte, error) {
					if readOffset != offset || readLength != length {
						return nil, ErrIntegrity
					}
					return store.Read(current, part.Hash, readLength)
				}
				return readLegacyByteSourcePart(ctx, dependency, ordinal)
			}
		}
		return live.Read(ctx, offset, length)
	}
	return restored
}

func (work *legacyNativeCheckpointNumberRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if !work.loaded {
		if err := work.checkBudget(4 * work.ref.Bytes); err != nil {
			return work.fail(err)
		}
		if err := legacyNativeCheckpointReadJSON(ctx, work.io, work.ref, &work.state); err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		if err := work.state.validate(); err != nil {
			return work.fail(err)
		}
		if work.state.SourceBytes != work.sourceBytes || work.state.Parser.SourceBound && work.authority == 0 {
			return work.fail(ErrIntegrity)
		}
		if work.state.needsSource() && (work.state.PartBytes != work.live.partBytes() || work.live.authority == 0) {
			return work.fail(ErrIntegrity)
		}
		if err := work.admitNative(); err != nil {
			return work.fail(err)
		}
		work.number = work.state.native(work.authority)
		work.authority = 0
		work.loaded = true
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if !work.sourceDone {
		return work.restoreSource(ctx)
	}
	if work.state.HasErrorText && work.number.errorText == nil {
		if work.scalar == nil {
			retained, err := work.retainedBytes()
			if err != nil {
				return work.fail(err)
			}
			if retained >= work.budget {
				return work.fail(ErrDocumentResourceLimit)
			}
			work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, work.state.ErrorText, work.io, work.sourceBytes, work.budget-retained)
			if err != nil {
				return work.fail(err)
			}
			if err := work.checkBudget(0); err != nil {
				return work.fail(err)
			}
			if err := ctx.Err(); err != nil {
				return work.fail(err)
			}
			return false, nil
		}
		ready, err := work.scalar.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.number.errorText, err = work.scalar.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.scalar.close()
			work.scalar = nil
			if err := work.validateText(); err != nil {
				return work.fail(err)
			}
		}
		if err := work.checkBudget(0); err != nil {
			return work.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.state.Semantic.Present {
		if err := work.checkBudget(int(unsafe.Sizeof(json.UnmarshalTypeError{}))); err != nil {
			return work.fail(err)
		}
		semantic := work.state.Semantic
		work.number.semantic = &json.UnmarshalTypeError{
			Value: work.number.errorText.text.String(), Type: reflect.TypeFor[float64](), Offset: semantic.Offset,
			Struct: semantic.Struct, Field: semantic.Field,
		}
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(0); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return true, nil
}

func (work *legacyNativeCheckpointNumberRestore) validateText() error {
	scalar := work.number.errorText
	saved := work.state.ErrorText.State
	if scalar == nil || scalar.text == nil || scalar.previous != nil || scalar.buffered != 0 ||
		scalar.position != saved.Position || scalar.end != saved.End || scalar.maximum != saved.Maximum || scalar.expanded != saved.Expanded ||
		scalar.text.Len() != saved.Bytes || scalar.text.Cap() < saved.Capacity || scalar.text.Len() < len("number ") ||
		scalar.text.String()[:len("number ")] != "number " {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeCheckpointNumberRestore) admitNative() error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	planned := docRangeHeaderHeapEstimate{bytes: retained}
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeNumberValue{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeFloat64{})))
	if work.state.Parser.HasDecimal {
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeFloat64Decimal{})))
	}
	if planned.err != nil {
		return planned.err
	}
	if planned.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointNumberRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.live.checkpointReaderHeap)
	if first, limit := work.state.sourceRange(); first != limit {
		heap.include(legacyNativeCheckpointNumberReaderHeap())
	}
	heap.allocation(int64(len(work.ref.Hash)), 1)
	heap.allocation(int64(len(work.state.Binding)), 1)
	heap.allocation(int64(len(work.state.ErrorText.Text.Tree.Ref.Hash)), 1)
	heap.allocation(int64(len(work.state.Semantic.Type)), 1)
	heap.allocation(int64(len(work.state.Semantic.Struct)), 1)
	heap.allocation(int64(len(work.state.Semantic.Field)), 1)
	heap.allocation(int64(len(work.state.SourceParts.Ref.Hash)), 1)
	heap.allocation(int64(len(work.live.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	heap.allocation(int64(len(work.live.Parts)), int64(len(contentHashPrefix)+64+32))
	if work.parts != nil {
		heap.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
		heap.allocation(int64(work.partCopied), int64(len(contentHashPrefix)+64+32))
	}
	if work.sourceParts != nil {
		retained, err := work.sourceParts.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.number != nil {
		work.number.retained(&heap)
	}
	if work.scalar != nil {
		retained, err := work.scalar.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointNumberRestore) checkBudget(scratch int) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.allocation(int64(scratch), 1)
	if heap.err != nil {
		return heap.err
	}
	if heap.bytes > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointNumberRestore) take(ctx context.Context) (*legacyNativeNumberValue, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if work.closed {
		return nil, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return nil, failure
	}
	if !work.ready || work.number == nil {
		_, err := work.fail(ErrIntegrity)
		return nil, err
	}
	number := work.number
	work.close()
	return number, nil
}

func (work *legacyNativeCheckpointNumberRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointNumberRestore) close() {
	if work != nil {
		if work.scalar != nil {
			work.scalar.close()
		}
		if work.sourceParts != nil {
			work.sourceParts.close()
		}
		failure := work.failure
		*work = legacyNativeCheckpointNumberRestore{closed: true, failure: failure}
	}
}
