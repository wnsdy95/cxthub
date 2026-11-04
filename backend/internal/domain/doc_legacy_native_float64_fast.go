package domain

import (
	"math"
	"math/bits"
)

func legacyNativeFloat64Fast(mantissa uint64, exponent int, negative, truncated bool) (bits uint64, ok bool) {
	if !truncated {
		if exactBits, exactOK := legacyNativeFloat64Exact(mantissa, exponent, negative); exactOK {
			return exactBits, true
		}
	}
	convertedBits, convertedOK := legacyNativeFloat64EiselLemire(mantissa, exponent, negative)
	if !convertedOK {
		return 0, false
	}
	if !truncated {
		return convertedBits, true
	}
	upperBits, upperOK := legacyNativeFloat64EiselLemire(mantissa+1, exponent, negative)
	if upperOK && math.Float64frombits(convertedBits) == math.Float64frombits(upperBits) {
		return convertedBits, true
	}
	return 0, false
}

func legacyNativeFloat64Exact(mantissa uint64, exponent int, negative bool) (uint64, bool) {
	if mantissa>>52 != 0 {
		return 0, false
	}
	value := float64(mantissa)
	if negative {
		value = -value
	}
	switch {
	case exponent == 0:
		return math.Float64bits(value), true
	case exponent > 0 && exponent <= 15+22:
		if exponent > 22 {
			value *= legacyNativeFloat64ExactPowers[exponent-22]
			exponent = 22
		}
		if value > 1e15 || value < -1e15 {
			return 0, false
		}
		return math.Float64bits(value * legacyNativeFloat64ExactPowers[exponent]), true
	case exponent < 0 && exponent >= -22:
		return math.Float64bits(value / legacyNativeFloat64ExactPowers[-exponent]), true
	}
	return 0, false
}

func legacyNativeFloat64EiselLemire(mantissa uint64, exponent int, negative bool) (uint64, bool) {
	if mantissa == 0 {
		if negative {
			return 0x8000000000000000, true
		}
		return 0, true
	}
	power, powerExponent, powerOK := legacyNativeFloat64Power(exponent)
	if !powerOK {
		return 0, false
	}
	leadingZeros := bits.LeadingZeros64(mantissa)
	mantissa <<= uint(leadingZeros)
	encodedExponent := uint64(powerExponent+63+1023) - uint64(leadingZeros)
	productHigh, productLow := bits.Mul64(mantissa, power.high)
	if productHigh&0x1ff == 0x1ff && productLow+mantissa < mantissa {
		lowerHigh, lowerLow := bits.Mul64(mantissa, power.low)
		mergedHigh, mergedLow := productHigh, productLow+lowerHigh
		if mergedLow < productLow {
			mergedHigh++
		}
		if mergedHigh&0x1ff == 0x1ff && mergedLow+1 == 0 && lowerLow+mantissa < mantissa {
			return 0, false
		}
		productHigh, productLow = mergedHigh, mergedLow
	}
	mostSignificantBit := productHigh >> 63
	resultMantissa := productHigh >> (mostSignificantBit + 9)
	encodedExponent -= 1 ^ mostSignificantBit
	if productLow == 0 && productHigh&0x1ff == 0 && resultMantissa&3 == 1 {
		return 0, false
	}
	resultMantissa += resultMantissa & 1
	resultMantissa >>= 1
	if resultMantissa>>53 > 0 {
		resultMantissa >>= 1
		encodedExponent += 1
	}
	if encodedExponent-1 >= 0x7ff-1 {
		return 0, false
	}
	resultBits := encodedExponent<<52 | resultMantissa&(1<<52-1)
	if negative {
		resultBits |= 0x8000000000000000
	}
	return resultBits, true
}
