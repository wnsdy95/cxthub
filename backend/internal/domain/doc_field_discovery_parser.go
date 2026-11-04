package domain

import "context"

var docFieldKeys = [...]string{"kind", "output", "redacted_summary", "blocks"}
var docFieldKinds = [...]string{string(EventMessage), string(EventReasoning), string(EventToolResult)}
var docBlockFieldKeys = [...]string{"type", "name", "text"}
var docBlockFieldKinds = [...]string{"text"}

func (schema docFieldSchema) keys() []string {
	if schema == docFieldSchemaBlock {
		return docBlockFieldKeys[:]
	}
	if schema == docFieldSchemaTool {
		return docToolFieldKeys[:]
	}
	return docFieldKeys[:]
}

func (schema docFieldSchema) kinds() []string {
	if schema == docFieldSchemaBlock {
		return docBlockFieldKinds[:]
	}
	if schema == docFieldSchemaTool {
		return docToolFieldKinds[:]
	}
	return docFieldKinds[:]
}

func (schema docFieldSchema) maxStringRunes() int {
	if schema == docFieldSchemaTool {
		return 19
	}
	return 17
}

func (state DocFieldDiscoveryState) validUTF8Progress() bool {
	switch state.UTF8Minimum {
	case 0x80:
		return state.UTF8Need == 1 && state.UTF8Value >= 2 && state.UTF8Value <= 31
	case 0x800:
		return state.UTF8Need == 2 && state.UTF8Value <= 15 || state.UTF8Need == 1 && state.UTF8Value >= 32 && state.UTF8Value <= 1023 && (state.UTF8Value < 0x360 || state.UTF8Value > 0x37f)
	case 0x10000:
		return state.UTF8Need == 3 && state.UTF8Value <= 4 || state.UTF8Need == 2 && state.UTF8Value >= 16 && state.UTF8Value <= 271 || state.UTF8Need == 1 && state.UTF8Value >= 1024 && state.UTF8Value <= 17407
	default:
		return false
	}
}

func (state *DocFieldDiscoveryState) consume(ctx context.Context, raw []byte) error {
	for index, character := range raw {
		if index%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for {
			consumed, err := state.accept(character)
			if err != nil {
				return err
			}
			if consumed {
				state.Next++
				break
			}
		}
	}
	return nil
}

func docFieldSpace(character byte) bool {
	return character == ' ' || character == '\n' || character == '\r' || character == '\t'
}

func (state *DocFieldDiscoveryState) clearToken() {
	state.Token, state.Number, state.Literal = 0, 0, 0
	state.Escape, state.HexDigits, state.HexValue = 0, 0, 0
	state.UTF8Value, state.UTF8Need, state.UTF8Minimum = 0, 0, 0
	state.StringPurpose, state.StringMask, state.StringRunes = 0, 0, 0
}

func (state *DocFieldDiscoveryState) observe(character rune) {
	if state.schema == docFieldSchemaEvent && state.Token == docFieldTokenString && len(state.Stack) == 1 && state.Field == docFieldOutput && !docMarkdownWhitespace(character) {
		state.OutputNonSpace = true
	}
	if state.StringPurpose == 0 {
		return
	}
	words := state.schema.keys()
	if state.StringPurpose == 2 {
		words = state.schema.kinds()
	}
	for index, word := range words {
		if state.StringMask&(1<<index) != 0 && (state.StringRunes >= len(word) || rune(word[state.StringRunes]) != character) {
			state.StringMask &^= 1 << index
		}
	}
	state.StringRunes = min(state.StringRunes+1, state.schema.maxStringRunes())
}

func (state *DocFieldDiscoveryState) startString(key bool) {
	state.Token = docFieldTokenString
	if key {
		state.Token = docFieldTokenKey
		if len(state.Stack) == 1 {
			state.StringPurpose, state.StringMask = 1, byte(1<<len(state.schema.keys()))-1
		}
	} else if len(state.Stack) == 1 && state.Field == docFieldKind {
		state.StringPurpose, state.StringMask = 2, byte(1<<len(state.schema.kinds()))-1
	}
}

func (state *DocFieldDiscoveryState) finishString() error {
	if state.Token == docFieldTokenKey {
		if len(state.Stack) == 1 {
			state.Field = docFieldUnknown
			for index, word := range state.schema.keys() {
				if state.StringMask&(1<<index) != 0 && state.StringRunes == len(word) {
					state.Field = byte(index + 1)
				}
			}
		}
		state.Stack[len(state.Stack)-1] = docFieldObjectColon
		state.clearToken()
		return nil
	}
	if state.StringPurpose == 2 {
		for index, word := range state.schema.kinds() {
			if state.StringMask&(1<<index) != 0 && state.StringRunes == len(word) {
				state.Kind = EventKind(word)
			}
		}
	}
	state.clearToken()
	return state.finishValue(state.Next + 1)
}

func (state *DocFieldDiscoveryState) stringByte(character byte) error {
	if state.UTF8Need > 0 {
		if character < 0x80 || character > 0xbf {
			return ErrIntegrity
		}
		state.UTF8Value = state.UTF8Value<<6 | uint32(character&0x3f)
		state.UTF8Need--
		if state.UTF8Need == 0 {
			if state.UTF8Value < state.UTF8Minimum || state.UTF8Value > 0x10ffff || state.UTF8Value >= 0xd800 && state.UTF8Value <= 0xdfff {
				return ErrIntegrity
			}
			state.observe(rune(state.UTF8Value))
			state.UTF8Value, state.UTF8Minimum = 0, 0
		}
		return nil
	}
	if state.Escape == 2 {
		value, valid := docDisplayHexDigit(character)
		if !valid {
			return ErrIntegrity
		}
		state.HexValue = state.HexValue*16 + uint32(value)
		state.HexDigits++
		if state.HexDigits == 4 {
			state.observe(rune(state.HexValue))
			state.Escape, state.HexDigits, state.HexValue = 0, 0, 0
		}
		return nil
	}
	if state.Escape == 1 {
		state.Escape = 0
		switch character {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			state.observe(docDisplaySimpleEscape(character))
		case 'u':
			state.Escape = 2
		default:
			return ErrIntegrity
		}
		return nil
	}
	switch {
	case character == '"':
		return state.finishString()
	case character == '\\':
		state.Escape = 1
	case character < 0x20:
		return ErrIntegrity
	case character < 0x80:
		state.observe(rune(character))
	case character >= 0xc2 && character <= 0xdf:
		state.UTF8Value, state.UTF8Need, state.UTF8Minimum = uint32(character&0x1f), 1, 0x80
	case character >= 0xe0 && character <= 0xef:
		state.UTF8Value, state.UTF8Need, state.UTF8Minimum = uint32(character&0x0f), 2, 0x800
	case character >= 0xf0 && character <= 0xf4:
		state.UTF8Value, state.UTF8Need, state.UTF8Minimum = uint32(character&0x07), 3, 0x10000
	default:
		return ErrIntegrity
	}
	return nil
}

func (state *DocFieldDiscoveryState) finishValue(end int) error {
	if len(state.Stack) == 0 {
		return ErrIntegrity
	}
	if len(state.Stack) == 1 {
		span := &DocFindSpan{From: state.ValueFrom, To: end}
		if state.schema == docFieldSchemaTool {
			state.finishToolFieldValue(span)
		} else {
			switch state.Field {
			case docFieldKind:
				if state.schema == docFieldSchemaBlock && state.ValueType == '"' {
					state.Blocks = span
				}
			case docFieldOutput:
				if state.ValueType == '"' {
					state.Output = span
				}
			case docFieldSummary:
				if state.ValueType == '"' {
					state.Summary = span
				}
			case docFieldBlocks:
				if state.ValueType == '[' {
					state.Blocks = span
				}
			}
		}
		state.Field, state.ValueFrom, state.ValueType = 0, 0, 0
	}
	switch state.Stack[len(state.Stack)-1] {
	case docFieldObjectValue:
		state.Stack[len(state.Stack)-1] = docFieldObjectAfter
	case docFieldArrayFirst, docFieldArrayValue:
		state.Stack[len(state.Stack)-1] = docFieldArrayAfter
	default:
		return ErrIntegrity
	}
	return nil
}

func (state *DocFieldDiscoveryState) closeContainer() error {
	state.Stack = state.Stack[:len(state.Stack)-1]
	if len(state.Stack) == 0 {
		state.RootDone = true
		return nil
	}
	return state.finishValue(state.Next + 1)
}

func (state *DocFieldDiscoveryState) startValue(character byte) error {
	if len(state.Stack) == 1 {
		state.ValueFrom, state.ValueType = state.Next, character
		if state.schema == docFieldSchemaTool {
			state.startToolFieldValue(character)
		} else {
			switch state.Field {
			case docFieldKind:
				state.Kind = ""
				if state.schema == docFieldSchemaBlock {
					state.Blocks, state.InvalidBlocks = nil, character != '"' && character != 'n'
				}
			case docFieldOutput:
				state.Output, state.OutputNonSpace = nil, false
				if state.schema == docFieldSchemaBlock {
					state.OutputNonSpace = character != '"' && character != 'n'
				}
			case docFieldSummary:
				state.Summary, state.InvalidSummary = nil, character != '"' && character != 'n'
			case docFieldBlocks:
				state.Blocks, state.InvalidBlocks = nil, character != '[' && character != 'n'
			}
		}
	}
	switch character {
	case '"':
		state.startString(false)
	case '{', '[':
		if len(state.Stack) >= docFieldDiscoveryDepth {
			return ErrIntegrity
		}
		frame := docFieldObjectFirst
		if character == '[' {
			frame = docFieldArrayFirst
		}
		state.Stack = append(state.Stack, frame)
	case 'n', 't', 'f':
		state.Token, state.Literal = docFieldTokenNull, 1
		if character == 't' {
			state.Token = docFieldTokenTrue
		} else if character == 'f' {
			state.Token = docFieldTokenFalse
		}
	case '-':
		state.Token, state.Number = docFieldTokenNumber, 1
	case '0':
		state.Token, state.Number = docFieldTokenNumber, 2
	default:
		if character < '1' || character > '9' {
			return ErrIntegrity
		}
		state.Token, state.Number = docFieldTokenNumber, 3
	}
	return nil
}

func (state *DocFieldDiscoveryState) numberByte(character byte) (bool, error) {
	digit := character >= '0' && character <= '9'
	switch state.Number {
	case 1:
		if character == '0' {
			state.Number = 2
		} else if digit {
			state.Number = 3
		} else {
			return false, ErrIntegrity
		}
		return true, nil
	case 2, 3:
		if state.Number == 3 && digit {
			return true, nil
		}
		if character == '.' {
			state.Number = 4
			return true, nil
		}
		if character == 'e' || character == 'E' {
			state.Number = 6
			return true, nil
		}
	case 4, 7:
		if !digit {
			return false, ErrIntegrity
		}
		state.Number++
		return true, nil
	case 5:
		if digit {
			return true, nil
		}
		if character == 'e' || character == 'E' {
			state.Number = 6
			return true, nil
		}
	case 6:
		if character == '+' || character == '-' {
			state.Number = 7
			return true, nil
		}
		if !digit {
			return false, ErrIntegrity
		}
		state.Number = 8
		return true, nil
	case 8:
		if digit {
			return true, nil
		}
	default:
		return false, ErrIntegrity
	}
	state.clearToken()
	return false, state.finishValue(state.Next)
}

func (state *DocFieldDiscoveryState) accept(character byte) (bool, error) {
	if state.Token == docFieldTokenKey || state.Token == docFieldTokenString {
		return true, state.stringByte(character)
	}
	if state.Token == docFieldTokenNumber {
		return state.numberByte(character)
	}
	if state.Token >= docFieldTokenNull {
		literal := "null"
		if state.Token == docFieldTokenTrue {
			literal = "true"
		} else if state.Token == docFieldTokenFalse {
			literal = "false"
		}
		if state.Literal >= len(literal) || character != literal[state.Literal] {
			return false, ErrIntegrity
		}
		state.Literal++
		if state.Literal == len(literal) {
			state.clearToken()
			return true, state.finishValue(state.Next + 1)
		}
		return true, nil
	}
	if docFieldSpace(character) {
		return true, nil
	}
	if !state.Started {
		if character != '{' {
			return false, ErrIntegrity
		}
		state.Started = true
		state.Stack = []byte{docFieldObjectFirst}
		return true, nil
	}
	if state.RootDone || len(state.Stack) == 0 {
		return false, ErrIntegrity
	}
	switch state.Stack[len(state.Stack)-1] {
	case docFieldObjectFirst, docFieldObjectKey:
		if character == '}' && state.Stack[len(state.Stack)-1] == docFieldObjectFirst {
			return true, state.closeContainer()
		}
		if character != '"' {
			return false, ErrIntegrity
		}
		state.startString(true)
	case docFieldObjectColon:
		if character != ':' {
			return false, ErrIntegrity
		}
		state.Stack[len(state.Stack)-1] = docFieldObjectValue
	case docFieldObjectValue, docFieldArrayFirst, docFieldArrayValue:
		if character == ']' && state.Stack[len(state.Stack)-1] == docFieldArrayFirst {
			return true, state.closeContainer()
		}
		return true, state.startValue(character)
	case docFieldObjectAfter:
		if character == '}' {
			return true, state.closeContainer()
		}
		if character != ',' {
			return false, ErrIntegrity
		}
		state.Stack[len(state.Stack)-1] = docFieldObjectKey
	case docFieldArrayAfter:
		if character == ']' {
			return true, state.closeContainer()
		}
		if character != ',' {
			return false, ErrIntegrity
		}
		state.Stack[len(state.Stack)-1] = docFieldArrayValue
	default:
		return false, ErrIntegrity
	}
	return true, nil
}
