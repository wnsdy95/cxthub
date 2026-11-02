package domain

func legacyJSONNumberNext(mode legacyJSONValueMode, current byte) (legacyJSONValueMode, bool) {
	switch mode {
	case legacyJSONValueIdle:
		if current == '-' {
			return legacyJSONValueMinus, true
		}
		fallthrough
	case legacyJSONValueMinus:
		if current == '0' {
			return legacyJSONValueZero, true
		}
		if current >= '1' && current <= '9' {
			return legacyJSONValueInteger, true
		}
	case legacyJSONValueZero, legacyJSONValueInteger, legacyJSONValueFraction, legacyJSONValueExponentDigits:
		if current >= '0' && current <= '9' {
			return mode, mode != legacyJSONValueZero
		}
		if current == '.' && (mode == legacyJSONValueZero || mode == legacyJSONValueInteger) {
			return legacyJSONValuePoint, true
		}
		if (current == 'e' || current == 'E') && mode != legacyJSONValueExponentDigits {
			return legacyJSONValueExponent, true
		}
	case legacyJSONValuePoint:
		if current >= '0' && current <= '9' {
			return legacyJSONValueFraction, true
		}
	case legacyJSONValueExponent:
		if current == '+' || current == '-' {
			return legacyJSONValueExponentSign, true
		}
		fallthrough
	case legacyJSONValueExponentSign:
		if current >= '0' && current <= '9' {
			return legacyJSONValueExponentDigits, true
		}
	}
	return mode, false
}

func legacyJSONNumberFinal(mode legacyJSONValueMode) bool {
	switch mode {
	case legacyJSONValueZero, legacyJSONValueInteger, legacyJSONValueFraction, legacyJSONValueExponentDigits:
		return true
	default:
		return false
	}
}
