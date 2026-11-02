package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"unicode/utf8"
	"unsafe"
)

const legacyNativeTypedNumberErrorPrefix = "json: invalid number literal, trying to unmarshal "
const legacyNativeTypedNumberErrorSuffix = " into Number"

type legacyNativeTypedNumber struct {
	scalar      *legacyNativeScalar
	errorText   *legacyNativeScalar
	semantic    error
	target      *json.Number
	authority   uint64
	sourceBytes int
	from        int
	end         int
	field       int
	validated   int
	mode        legacyJSONValueMode
	diagnostics legacyNativeNumberDiagnostics
	quoted      bool
	decoded     bool
	rejected    bool
	complete    bool
	newTarget   bool
}

func (native *legacyNativeObject) typedNumberField(field legacyNativeObjectField) bool {
	return field.typedNumber && native.valueEnd-native.valueStart > LegacyCanonicalEncodingBytes &&
		(native.valueFirst == '"' || native.valueFirst == '-' || native.valueFirst >= '0' && native.valueFirst <= '9')
}

func (native *legacyNativeObject) decodeTypedNumber(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if native == nil || input == nil || native.metadata == nil || native.field < 0 || native.field >= len(native.metadata.fields) ||
		len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) ||
		input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads < 0 || input.loads > 2 ||
		native.phase != legacyNativeObjectApply || native.valueStart < 0 || native.valueEnd > input.source.Bytes || native.position != native.valueEnd ||
		input.source.authority == 0 || (input.numberDiagnostics != legacyNativeNumberDiagnosticsFull && input.numberDiagnostics != legacyNativeNumberDiagnosticsRejectOnly) {
		return false, ErrIntegrity
	}
	if input.failure != nil {
		return false, input.failure
	}
	field := native.metadata.fields[native.field]
	if !native.typedNumberField(field) {
		return false, ErrIntegrity
	}
	value := native.destination.Elem().Field(field.index)
	if value.Type() != reflect.TypeFor[*json.Number]() {
		return false, ErrIntegrity
	}
	if native.typedNumber == nil {
		cost := 2 * int(unsafe.Sizeof(legacyNativeTypedNumber{})+unsafe.Sizeof(legacyNativeScalar{})+unsafe.Sizeof(json.Number(""))+unsafe.Sizeof(docRangeHeaderHeapEstimate{}))
		if cost > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		length := native.valueEnd - native.valueStart
		if length > (int(^uint(0)>>1)-len(legacyNativeTypedNumberErrorPrefix)-len(legacyNativeTypedNumberErrorSuffix)-2)/6 {
			return false, ErrIntegrity
		}
		number := &legacyNativeTypedNumber{
			authority: input.source.authority, sourceBytes: input.source.Bytes,
			from: native.valueStart, end: native.valueEnd, field: native.field,
			diagnostics: input.numberDiagnostics, quoted: native.valueFirst == '"',
		}
		from, to, maximum := number.from, number.end, length
		if number.quoted {
			from, to, maximum = from+1, to-1, 3*(length-2)
		}
		number.scalar = &legacyNativeScalar{position: from, end: to, maximum: maximum}
		number.target, _ = value.Interface().(*json.Number)
		if number.target == nil {
			number.target = new(json.Number)
			number.newTarget = true
		}
		var heap docRangeHeaderHeapEstimate
		heap.allocation(1, int64(value.Type().Size()))
		if !number.newTarget {
			heap.allocation(1, int64(unsafe.Sizeof(*number.target)))
			heap.allocation(int64(len(*number.target)), 1)
			heap.include(docRangeHeaderHeapEstimate{bytes: native.backingExcess[native.field]})
		}
		if heap.err != nil {
			return false, heap.err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		native.heaps[native.field], native.typedNumber = heap, number
		input.workBytes += cost
	}
	number := native.typedNumber
	if number.authority != input.source.authority || number.sourceBytes != input.source.Bytes || number.diagnostics != input.numberDiagnostics ||
		number.from != native.valueStart || number.end != native.valueEnd || number.field != native.field ||
		number.quoted != (native.valueFirst == '"') ||
		number.newTarget && !value.IsNil() || !number.newTarget && value.Interface().(*json.Number) != number.target {
		return false, ErrIntegrity
	}
	if number.rejected {
		return native.rejectTypedNumber(ctx, input, number, value)
	}
	if !number.decoded {
		complete, err := number.scalar.step(ctx, input)
		if err != nil || !complete {
			return false, err
		}
		number.decoded = true
	}
	text := number.scalar.text.String()
	limit := min(ConversationManifestChunkBytes, input.workBytes+canonicalCheckBytes)
	for number.validated < len(text) && input.workBytes < limit {
		mode, valid := legacyJSONNumberNext(number.mode, text[number.validated])
		input.workBytes++
		if !valid {
			if !number.quoted {
				return false, ErrIntegrity
			}
			number.rejected, number.scalar = true, nil
			return native.rejectTypedNumber(ctx, input, number, value)
		}
		number.mode = mode
		number.validated++
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if number.validated != len(text) {
		return false, nil
	}
	if !legacyJSONNumberFinal(number.mode) {
		if !number.quoted {
			return false, ErrIntegrity
		}
		number.rejected, number.scalar = true, nil
		return native.rejectTypedNumber(ctx, input, number, value)
	}
	cost := 2 * int(unsafe.Sizeof(*number.target)+unsafe.Sizeof(docRangeHeaderHeapEstimate{})+unsafe.Sizeof(int64(0)))
	if cost > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(value.Type().Size()))
	heap.allocation(1, int64(unsafe.Sizeof(*number.target)))
	heap.allocation(int64(number.scalar.text.Cap()), 1)
	if heap.err != nil {
		return false, heap.err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	*number.target = json.Number(text)
	if number.newTarget {
		value.Set(reflect.ValueOf(number.target))
		number.newTarget = false
	}
	native.heaps[native.field] = heap
	native.backingExcess[native.field] = 2 * int64(number.scalar.text.Cap()-len(text))
	native.typedNumber = nil
	input.workBytes += cost
	return true, nil
}

func (native *legacyNativeObject) rejectTypedNumber(ctx context.Context, input *legacyNormalizationInput, number *legacyNativeTypedNumber, value reflect.Value) (bool, error) {
	complete, failure := number.reject(ctx, input)
	if !complete {
		return false, failure
	}
	if input.failure != nil {
		return false, input.failure
	}
	if failure == nil || failure != errLegacyNativeNumberRejected && failure != number.semantic {
		if failure == nil {
			return false, ErrIntegrity
		}
		return false, failure
	}
	if number.newTarget {
		cost := 2 * int(unsafe.Sizeof(*number.target)+unsafe.Sizeof(docRangeHeaderHeapEstimate{}))
		if cost > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		var heap docRangeHeaderHeapEstimate
		heap.allocation(1, int64(value.Type().Size()))
		heap.allocation(1, int64(unsafe.Sizeof(*number.target)))
		if heap.err != nil {
			return false, heap.err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		value.Set(reflect.ValueOf(number.target))
		native.heaps[native.field], native.backingExcess[native.field] = heap, 0
		number.newTarget = false
		input.workBytes += cost
		return true, failure
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, failure
}

func (number *legacyNativeTypedNumber) reject(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if number.diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
		number.complete = true
		return true, errLegacyNativeNumberRejected
	}
	if number.complete {
		return true, number.semantic
	}
	if number.errorText == nil {
		cost := len(legacyNativeTypedNumberErrorPrefix) + 1 + 2*int(unsafe.Sizeof(legacyNativeScalar{}))
		if cost > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		number.errorText = &legacyNativeScalar{
			position: number.from, end: number.end, expanded: true,
			maximum: 6*(number.end-number.from) + len(legacyNativeTypedNumberErrorPrefix) + len(legacyNativeTypedNumberErrorSuffix) + 2,
		}
		ready, err := number.errorText.reserve(ctx, input, len(legacyNativeTypedNumberErrorPrefix)+1)
		if err != nil || !ready {
			return false, err
		}
		_, _ = number.errorText.text.WriteString(legacyNativeTypedNumberErrorPrefix)
		_ = number.errorText.text.WriteByte('"')
		input.workBytes += cost
	}
	scalar := number.errorText
	var scratch [6*12 + 2]byte
	for scalar.position < scalar.end || scalar.buffered != 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ready, err := scalar.reserve(ctx, input, 6*len(scalar.pending))
		if err != nil || !ready {
			return false, err
		}
		ready, err = scalar.fill(input)
		if err != nil || !ready {
			return false, err
		}
		if 8*scalar.buffered+2 > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		length := scalar.buffered
		for offset := 0; offset < length; {
			if !utf8.FullRune(scalar.pending[offset:length]) && scalar.position+length < scalar.end {
				length = offset
				break
			}
			_, width := utf8.DecodeRune(scalar.pending[offset:length])
			offset += width
		}
		quoted := strconv.AppendQuote(scratch[:0], string(scalar.pending[:length]))
		input.workBytes += 2 * length
		encoded := quoted[1 : len(quoted)-1]
		_, _ = scalar.text.Write(encoded)
		input.workBytes += len(encoded)
		scalar.position += length
		copy(scalar.pending[:], scalar.pending[length:scalar.buffered])
		scalar.buffered -= length
	}
	cost := len(legacyNativeTypedNumberErrorSuffix) + 1 + int(unsafe.Sizeof(string("")))
	if cost > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	ready, err := scalar.reserve(ctx, input, len(legacyNativeTypedNumberErrorSuffix)+1)
	if err != nil || !ready {
		return false, err
	}
	if cost > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_ = scalar.text.WriteByte('"')
	_, _ = scalar.text.WriteString(legacyNativeTypedNumberErrorSuffix)
	input.workBytes += cost
	number.semantic = errors.New(scalar.text.String())
	number.complete = true
	return true, number.semantic
}

func (number *legacyNativeTypedNumber) retained(estimate *docRangeHeaderHeapEstimate) {
	if number == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*number)))
	if number.newTarget {
		estimate.allocation(1, int64(unsafe.Sizeof(*number.target)))
	}
	if number.scalar != nil {
		number.scalar.retained(estimate)
	}
	if number.errorText != nil {
		number.errorText.retained(estimate)
	}
	if number.semantic != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(string(""))))
	}
}
