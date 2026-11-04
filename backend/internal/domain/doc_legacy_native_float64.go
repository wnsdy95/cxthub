package domain

import (
	"context"
	"errors"
	"unsafe"
)

const legacyNativeFloat64Bytes = 64 << 10

const (
	legacyNativeFloat64Start uint8 = iota
	legacyNativeFloat64IntegerFirst
	legacyNativeFloat64IntegerZero
	legacyNativeFloat64Integer
	legacyNativeFloat64FractionFirst
	legacyNativeFloat64Fraction
	legacyNativeFloat64ExponentFirst
	legacyNativeFloat64ExponentDigitFirst
	legacyNativeFloat64Exponent
	legacyNativeFloat64Convert
	legacyNativeFloat64Done
)

type legacyNativeFloat64 struct {
	digits         [800]byte
	decimal        *legacyNativeFloat64Decimal
	mantissa       uint64
	bits           uint64
	authority      uint64
	sourceBytes    int
	from           int
	position       int
	end            int
	significant    int
	mantissaDigits int
	retainedDigits int
	primaryPoint   int
	decimalPoint   int
	exponent       int
	phase          uint8
	negative       bool
	negativeExp    bool
	dot            bool
	primaryTrunc   bool
	decimalTrunc   bool
	rejected       bool
}

func newLegacyNativeFloat64(from, to int) (*legacyNativeFloat64, error) {
	if from < 0 || to <= from || to-from > int(^uint(0)>>1)-(99999+19) {
		return nil, ErrIntegrity
	}
	return &legacyNativeFloat64{from: from, position: from, end: to}, nil
}

func (parser *legacyNativeFloat64) digit(current byte) {
	if current == '0' && parser.significant == 0 {
		parser.primaryPoint--
		parser.decimalPoint--
		return
	}
	parser.significant++
	if parser.mantissaDigits < 19 {
		parser.mantissa = parser.mantissa*10 + uint64(current-'0')
		parser.mantissaDigits++
	} else if current != '0' {
		parser.primaryTrunc = true
	}
	if parser.retainedDigits < len(parser.digits) {
		parser.digits[parser.retainedDigits] = current
		parser.retainedDigits++
	} else if current != '0' {
		parser.decimalTrunc = true
	}
}

func (parser *legacyNativeFloat64) decimalMark() {
	parser.dot = true
	parser.primaryPoint, parser.decimalPoint = parser.significant, parser.retainedDigits
	parser.phase = legacyNativeFloat64FractionFirst
}

func (parser *legacyNativeFloat64) exponentDigit(current byte) {
	if parser.exponent < 10000 {
		parser.exponent = 10*parser.exponent + int(current-'0')
	}
	parser.phase = legacyNativeFloat64Exponent
}

func (parser *legacyNativeFloat64) add(current byte) error {
	digit := current >= '0' && current <= '9'
	switch parser.phase {
	case legacyNativeFloat64Start:
		if current == '-' {
			parser.negative, parser.phase = true, legacyNativeFloat64IntegerFirst
			return nil
		}
		fallthrough
	case legacyNativeFloat64IntegerFirst:
		if !digit {
			return ErrIntegrity
		}
		parser.digit(current)
		parser.phase = legacyNativeFloat64Integer
		if current == '0' {
			parser.phase = legacyNativeFloat64IntegerZero
		}
	case legacyNativeFloat64IntegerZero, legacyNativeFloat64Integer:
		if digit && parser.phase == legacyNativeFloat64Integer {
			parser.digit(current)
		} else if current == '.' {
			parser.decimalMark()
		} else if current == 'e' || current == 'E' {
			parser.phase = legacyNativeFloat64ExponentFirst
		} else {
			return ErrIntegrity
		}
	case legacyNativeFloat64FractionFirst:
		if !digit {
			return ErrIntegrity
		}
		parser.digit(current)
		parser.phase = legacyNativeFloat64Fraction
	case legacyNativeFloat64Fraction:
		if digit {
			parser.digit(current)
		} else if current == 'e' || current == 'E' {
			parser.phase = legacyNativeFloat64ExponentFirst
		} else {
			return ErrIntegrity
		}
	case legacyNativeFloat64ExponentFirst:
		if current == '+' || current == '-' {
			parser.negativeExp, parser.phase = current == '-', legacyNativeFloat64ExponentDigitFirst
		} else if digit {
			parser.exponentDigit(current)
		} else {
			return ErrIntegrity
		}
	case legacyNativeFloat64ExponentDigitFirst, legacyNativeFloat64Exponent:
		if !digit {
			return ErrIntegrity
		}
		parser.exponentDigit(current)
	default:
		return ErrIntegrity
	}
	return nil
}

func (parser *legacyNativeFloat64) finish() error {
	switch parser.phase {
	case legacyNativeFloat64IntegerZero, legacyNativeFloat64Integer, legacyNativeFloat64Fraction, legacyNativeFloat64Exponent:
	default:
		return ErrIntegrity
	}
	if !parser.dot {
		parser.primaryPoint, parser.decimalPoint = parser.significant, parser.retainedDigits
	}
	exponent := parser.exponent
	if parser.negativeExp {
		exponent = -exponent
	}
	parser.primaryPoint += exponent
	parser.decimalPoint += exponent
	parser.phase = legacyNativeFloat64Convert
	return nil
}

func (parser *legacyNativeFloat64) step(ctx context.Context, input *legacyNormalizationInput) (uint64, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, false, err
	}
	if parser == nil || input == nil || input.unbounded || parser.from < 0 || parser.end <= parser.from ||
		parser.end > input.source.Bytes || parser.position < parser.from || parser.position > parser.end ||
		parser.phase > legacyNativeFloat64Done || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes {
		return 0, false, false, ErrIntegrity
	}
	if input.failure != nil {
		return 0, false, false, input.failure
	}
	if input.source.authority == 0 || parser.authority != 0 &&
		(parser.authority != input.source.authority || parser.sourceBytes != input.source.Bytes) {
		return 0, false, false, ErrIntegrity
	}
	if parser.phase == legacyNativeFloat64Done {
		return parser.bits, true, parser.rejected, nil
	}
	cost := 2 * int(unsafe.Sizeof(*parser))
	if cost > ConversationManifestChunkBytes-input.workBytes {
		return 0, false, false, nil
	}
	next := *parser
	input.workBytes += cost
	limit := min(ConversationManifestChunkBytes, input.workBytes+legacyNativeFloat64Bytes-cost)
	for next.position < next.end && input.workBytes < limit {
		current, err := input.current(next.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, false, false, ctxErr
			}
			return 0, false, false, err
		}
		if err := next.add(current); err != nil {
			return 0, false, false, err
		}
		next.position++
		next.authority, next.sourceBytes = input.source.authority, input.source.Bytes
		input.semanticBytes++
		if (next.position-next.from)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, false, err
			}
		}
	}
	if next.position == next.end && next.phase < legacyNativeFloat64Convert {
		if err := next.finish(); err != nil {
			return 0, false, false, err
		}
	}
	if next.phase == legacyNativeFloat64Convert && next.decimal == nil {
		conversionCost := 4*len(next.digits) + 128
		if conversionCost > limit-input.workBytes {
			if err := ctx.Err(); err != nil {
				return 0, false, false, err
			}
			*parser = next
			return 0, false, false, nil
		}
		input.workBytes += conversionCost
		exponent := 0
		if next.mantissa != 0 {
			exponent = next.primaryPoint - next.mantissaDigits
		}
		if bits, ok := legacyNativeFloat64Fast(next.mantissa, exponent, next.negative, next.primaryTrunc); ok {
			next.bits, next.phase = bits, legacyNativeFloat64Done
		} else {
			decimal, err := newLegacyNativeFloat64Decimal(next.digits, next.retainedDigits, next.decimalPoint, next.negative, next.decimalTrunc)
			if err != nil {
				return 0, false, false, err
			}
			next.decimal = decimal
		}
	} else if next.phase == legacyNativeFloat64Convert {
		copyCost := 2 * int(unsafe.Sizeof(*next.decimal))
		if copyCost > limit-input.workBytes {
			return 0, false, false, ctx.Err()
		}
		decimal := *next.decimal
		input.workBytes += copyCost
		bits, complete, rejected, err := decimal.step(ctx, input)
		if err != nil {
			return 0, false, false, err
		}
		next.decimal = &decimal
		if complete {
			next.bits, next.rejected, next.phase = bits, rejected, legacyNativeFloat64Done
			if rejected {
				next.bits = 0
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, false, false, err
	}
	*parser = next
	return parser.bits, parser.phase == legacyNativeFloat64Done, parser.rejected, nil
}

func (parser *legacyNativeFloat64) retained(estimate *docRangeHeaderHeapEstimate) {
	if parser == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*parser)))
	if parser.decimal != nil {
		parser.decimal.retained(estimate)
	}
}
