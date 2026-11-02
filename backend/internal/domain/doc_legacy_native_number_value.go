package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"unsafe"
)

type legacyNativeNumberDiagnostics uint8

const (
	legacyNativeNumberDiagnosticsFull legacyNativeNumberDiagnostics = iota
	legacyNativeNumberDiagnosticsRejectOnly
)

var errLegacyNativeNumberRejected = errors.New("legacy native number rejected")

type legacyNativeNumberValue struct {
	parser      *legacyNativeFloat64
	errorText   *legacyNativeScalar
	semantic    *json.UnmarshalTypeError
	value       float64
	converted   bool
	rejected    bool
	complete    bool
	diagnostics legacyNativeNumberDiagnostics
}

func newLegacyNativeNumberValue(from, to int, diagnostics legacyNativeNumberDiagnostics) (*legacyNativeNumberValue, error) {
	if diagnostics != legacyNativeNumberDiagnosticsFull && diagnostics != legacyNativeNumberDiagnosticsRejectOnly {
		return nil, ErrIntegrity
	}
	parser, err := newLegacyNativeFloat64(from, to)
	if err != nil {
		return nil, err
	}
	return &legacyNativeNumberValue{parser: parser, diagnostics: diagnostics}, nil
}

func (number *legacyNativeNumberValue) step(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if number == nil || number.parser == nil || input == nil {
		return false, ErrIntegrity
	}
	if number.diagnostics != input.numberDiagnostics {
		return false, ErrIntegrity
	}
	for !number.converted {
		work := input.workBytes
		bits, complete, rejected, err := number.parser.step(ctx, input)
		if err != nil {
			return false, err
		}
		if complete {
			number.value, number.converted, number.rejected = math.Float64frombits(bits), true, rejected
		} else if input.workBytes == work || input.workBytes >= ConversationManifestChunkBytes {
			return false, nil
		}
	}
	if number.rejected && number.diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		number.complete = true
		return true, errLegacyNativeNumberRejected
	}
	if number.rejected && !number.complete {
		if number.errorText == nil {
			if ConversationManifestChunkBytes-input.workBytes < 135 {
				return false, nil
			}
			text := &strings.Builder{}
			maximum := number.parser.end - number.parser.from + len("number ")
			text.Grow(min(128, maximum))
			_, _ = text.WriteString("number ")
			number.errorText = &legacyNativeScalar{text: text, position: number.parser.from, end: number.parser.end, maximum: maximum}
			input.workBytes += 135
		}
		ready, err := number.errorText.step(ctx, input)
		if err != nil || !ready {
			return false, err
		}
		number.semantic = &json.UnmarshalTypeError{
			Value: number.errorText.text.String(), Type: reflect.TypeFor[float64](), Offset: int64(number.parser.end-number.parser.from) + 1,
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	number.complete = true
	if number.semantic != nil {
		return true, number.semantic
	}
	return true, nil
}

func (number *legacyNativeNumberValue) retained(estimate *docRangeHeaderHeapEstimate) {
	if number == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*number)))
	number.parser.retained(estimate)
	if number.errorText != nil {
		number.errorText.retained(estimate)
	}
	if number.semantic != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*number.semantic)))
	}
}
