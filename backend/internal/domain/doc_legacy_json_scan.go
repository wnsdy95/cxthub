package domain

type legacyJSONValueFrame uint8

const (
	legacyJSONValueObjectFirst legacyJSONValueFrame = iota
	legacyJSONValueObjectKey
	legacyJSONValueObjectColon
	legacyJSONValueObjectValue
	legacyJSONValueObjectAfter
	legacyJSONValueArrayFirst
	legacyJSONValueArrayValue
	legacyJSONValueArrayAfter
)

type legacyJSONValueMode uint8

const (
	legacyJSONValueIdle legacyJSONValueMode = iota
	legacyJSONValueString
	legacyJSONValueEscape
	legacyJSONValueUnicode
	legacyJSONValueLiteral
	legacyJSONValueMinus
	legacyJSONValueZero
	legacyJSONValueInteger
	legacyJSONValuePoint
	legacyJSONValueFraction
	legacyJSONValueExponent
	legacyJSONValueExponentSign
	legacyJSONValueExponentDigits
)

type legacyJSONValueScanner struct {
	stack       [10000]legacyJSONValueFrame
	parents     int
	depth       int
	mode        legacyJSONValueMode
	literal     string
	literalNext uint8
	unicodeLeft uint8
	key         bool
	complete    bool
	invalid     bool
}

func (scanner *legacyJSONValueScanner) reset(parents int) error {
	scanner.parents = parents
	scanner.depth = 0
	scanner.mode = legacyJSONValueIdle
	scanner.literal = ""
	scanner.literalNext = 0
	scanner.unicodeLeft = 0
	scanner.key = false
	scanner.complete = false
	scanner.invalid = parents < 0 || parents > len(scanner.stack)
	if scanner.invalid {
		return ErrIntegrity
	}
	return nil
}

func legacyJSONValueSpace(current byte) bool {
	return current == ' ' || current == '\t' || current == '\r' || current == '\n'
}

func legacyJSONValueDelimiter(current byte) bool {
	return legacyJSONValueSpace(current) || current == ',' || current == ']' || current == '}'
}

func (scanner *legacyJSONValueScanner) reject() (bool, bool, error) {
	scanner.invalid = true
	return false, false, ErrIntegrity
}

func (scanner *legacyJSONValueScanner) endValue() {
	scanner.mode = legacyJSONValueIdle
	if scanner.depth == 0 {
		scanner.complete = true
		return
	}
	if scanner.stack[scanner.depth-1] == legacyJSONValueObjectValue {
		scanner.stack[scanner.depth-1] = legacyJSONValueObjectAfter
	} else {
		scanner.stack[scanner.depth-1] = legacyJSONValueArrayAfter
	}
}

func (scanner *legacyJSONValueScanner) closeValue() (bool, bool, error) {
	scanner.depth--
	scanner.endValue()
	return true, scanner.complete, nil
}

func (scanner *legacyJSONValueScanner) add(current byte) (consumed bool, complete bool, err error) {
	if scanner.invalid {
		return false, false, ErrIntegrity
	}
	if scanner.complete {
		return false, true, nil
	}
	switch scanner.mode {
	case legacyJSONValueString:
		switch {
		case current == '"':
			if scanner.key {
				scanner.key = false
				scanner.mode = legacyJSONValueIdle
				scanner.stack[scanner.depth-1] = legacyJSONValueObjectColon
			} else {
				scanner.endValue()
			}
		case current == '\\':
			scanner.mode = legacyJSONValueEscape
		case current < 0x20:
			return scanner.reject()
		}
		return true, scanner.complete, nil
	case legacyJSONValueEscape:
		switch current {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			scanner.mode = legacyJSONValueString
		case 'u':
			scanner.mode = legacyJSONValueUnicode
			scanner.unicodeLeft = 4
		default:
			return scanner.reject()
		}
		return true, false, nil
	case legacyJSONValueUnicode:
		if !(current >= '0' && current <= '9' || current >= 'a' && current <= 'f' || current >= 'A' && current <= 'F') {
			return scanner.reject()
		}
		scanner.unicodeLeft--
		if scanner.unicodeLeft == 0 {
			scanner.mode = legacyJSONValueString
		}
		return true, false, nil
	case legacyJSONValueLiteral:
		if current != scanner.literal[scanner.literalNext] {
			return scanner.reject()
		}
		scanner.literalNext++
		if int(scanner.literalNext) == len(scanner.literal) {
			scanner.endValue()
		}
		return true, scanner.complete, nil
	case legacyJSONValueMinus, legacyJSONValueZero, legacyJSONValueInteger, legacyJSONValuePoint,
		legacyJSONValueFraction, legacyJSONValueExponent, legacyJSONValueExponentSign, legacyJSONValueExponentDigits:
		if next, valid := legacyJSONNumberNext(scanner.mode, current); valid {
			scanner.mode = next
			return true, false, nil
		}
		if !legacyJSONNumberFinal(scanner.mode) || !legacyJSONValueDelimiter(current) {
			return scanner.reject()
		}
		scanner.endValue()
		if scanner.complete {
			return false, true, nil
		}
	}
	if legacyJSONValueSpace(current) {
		return true, false, nil
	}
	if scanner.depth > 0 {
		switch scanner.stack[scanner.depth-1] {
		case legacyJSONValueObjectFirst, legacyJSONValueObjectKey:
			if current == '}' && scanner.stack[scanner.depth-1] == legacyJSONValueObjectFirst {
				return scanner.closeValue()
			}
			if current != '"' {
				return scanner.reject()
			}
			scanner.key = true
			scanner.mode = legacyJSONValueString
			return true, false, nil
		case legacyJSONValueObjectColon:
			if current != ':' {
				return scanner.reject()
			}
			scanner.stack[scanner.depth-1] = legacyJSONValueObjectValue
			return true, false, nil
		case legacyJSONValueObjectAfter:
			switch current {
			case ',':
				scanner.stack[scanner.depth-1] = legacyJSONValueObjectKey
				return true, false, nil
			case '}':
				return scanner.closeValue()
			default:
				return scanner.reject()
			}
		case legacyJSONValueArrayFirst:
			if current == ']' {
				return scanner.closeValue()
			}
		case legacyJSONValueArrayAfter:
			switch current {
			case ',':
				scanner.stack[scanner.depth-1] = legacyJSONValueArrayValue
				return true, false, nil
			case ']':
				return scanner.closeValue()
			default:
				return scanner.reject()
			}
		}
	}
	switch current {
	case '{', '[':
		if scanner.depth == len(scanner.stack)-scanner.parents {
			return scanner.reject()
		}
		frame := legacyJSONValueObjectFirst
		if current == '[' {
			frame = legacyJSONValueArrayFirst
		}
		scanner.stack[scanner.depth] = frame
		scanner.depth++
	case '"':
		scanner.mode = legacyJSONValueString
	case 't', 'f', 'n':
		scanner.mode = legacyJSONValueLiteral
		scanner.literalNext = 1
		switch current {
		case 't':
			scanner.literal = "true"
		case 'f':
			scanner.literal = "false"
		case 'n':
			scanner.literal = "null"
		}
	default:
		next, valid := legacyJSONNumberNext(legacyJSONValueIdle, current)
		if !valid {
			return scanner.reject()
		}
		scanner.mode = next
	}
	return true, false, nil
}

func (scanner *legacyJSONValueScanner) finish() (complete bool, err error) {
	if scanner.invalid {
		return false, ErrIntegrity
	}
	if scanner.complete {
		return true, nil
	}
	if scanner.depth == 0 && legacyJSONNumberFinal(scanner.mode) {
		scanner.endValue()
		return true, nil
	}
	scanner.invalid = true
	return false, ErrIntegrity
}
