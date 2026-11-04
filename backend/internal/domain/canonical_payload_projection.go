package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

type canonicalPayloadFrame struct {
	kind     byte
	state    byte
	keyStart int
	keyEnd   int
}

type canonicalPayloadParser struct {
	ctx      context.Context
	raw      []byte
	offset   int
	checked  int
	maxDepth int
	frames   []canonicalPayloadFrame
}

func canonicalPayloadProjection(ctx context.Context, raw []byte, start, maxDepth int) (int, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if start < 0 || start >= len(raw) || maxDepth < 0 || maxDepth > 10000 {
		return 0, nil, ErrIntegrity
	}
	var initial [16]canonicalPayloadFrame
	parser := canonicalPayloadParser{ctx: ctx, raw: raw, offset: start, checked: start, maxDepth: maxDepth, frames: initial[:0]}
	if err := parser.value(); err != nil {
		return 0, nil, err
	}
	for len(parser.frames) > 0 {
		if err := parser.step(); err != nil {
			return 0, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if parser.offset < len(raw) && raw[parser.offset] != ',' && raw[parser.offset] != ']' && raw[parser.offset] != '}' {
		return 0, nil, ErrIntegrity
	}
	var marker []byte
	switch raw[start] {
	case '{':
		marker = []byte(`{}`)
	case '[':
		marker = []byte(`[]`)
	case '"':
		marker = []byte(`"x"`)
		if parser.offset-start == 2 {
			marker = []byte(`""`)
		}
	case 'n':
		marker = []byte(`null`)
	case 't', 'f':
		marker = []byte(`false`)
	default:
		marker = []byte(`0`)
	}
	return parser.offset, marker, nil
}

func (parser *canonicalPayloadParser) check() error {
	if parser.offset-parser.checked < canonicalCheckBytes-64 {
		return nil
	}
	parser.checked = parser.offset
	return parser.ctx.Err()
}

func (parser *canonicalPayloadParser) value() error {
	if err := parser.check(); err != nil {
		return err
	}
	if parser.offset >= len(parser.raw) {
		return ErrIntegrity
	}
	current := parser.raw[parser.offset]
	switch current {
	case '{', '[':
		if len(parser.frames) == parser.maxDepth {
			return ErrIntegrity
		}
		parser.frames = append(parser.frames, canonicalPayloadFrame{kind: current})
		parser.offset++
		return nil
	case '"':
		end, err := canonicalPayloadString(parser.ctx, parser.raw, parser.offset)
		parser.offset = end
		parser.checked = end
		return err
	case 'n', 't', 'f':
		literal := "null"
		if current == 't' {
			literal = "true"
		} else if current == 'f' {
			literal = "false"
		}
		if !bytes.HasPrefix(parser.raw[parser.offset:], []byte(literal)) {
			return ErrIntegrity
		}
		parser.offset += len(literal)
		return nil
	default:
		if current != '-' && (current < '0' || current > '9') {
			return ErrIntegrity
		}
		start := parser.offset
		for parser.offset < len(parser.raw) {
			current = parser.raw[parser.offset]
			if current != '-' && current != '+' && current != '.' && current != 'e' && current != 'E' && (current < '0' || current > '9') {
				break
			}
			parser.offset++
			if parser.offset-start > 64 {
				return ErrIntegrity
			}
		}
		var number float64
		token := parser.raw[start:parser.offset]
		if json.Unmarshal(token, &number) != nil {
			return ErrIntegrity
		}
		canonical, err := json.Marshal(number)
		if err != nil || !bytes.Equal(canonical, token) {
			return ErrIntegrity
		}
		return nil
	}
}

func (parser *canonicalPayloadParser) step() error {
	if err := parser.check(); err != nil {
		return err
	}
	if parser.offset >= len(parser.raw) {
		return ErrIntegrity
	}
	frame := &parser.frames[len(parser.frames)-1]
	current := parser.raw[parser.offset]
	if frame.kind == '{' && frame.state <= 1 {
		if current == '}' && frame.state == 0 {
			parser.frames = parser.frames[:len(parser.frames)-1]
			parser.offset++
			return nil
		}
		if current != '"' {
			return ErrIntegrity
		}
		start := parser.offset
		end, err := canonicalPayloadString(parser.ctx, parser.raw, start)
		if err != nil {
			return err
		}
		if frame.keyEnd != 0 {
			ordered, err := canonicalPayloadKeysOrdered(parser.ctx, parser.raw, frame.keyStart, frame.keyEnd, start, end)
			if err != nil {
				return err
			}
			if !ordered {
				return ErrIntegrity
			}
		}
		if end >= len(parser.raw) || parser.raw[end] != ':' {
			return ErrIntegrity
		}
		frame.keyStart, frame.keyEnd, frame.state = start, end, 2
		parser.offset, parser.checked = end+1, end
		return nil
	}
	if frame.state == 3 {
		closing := byte(']')
		if frame.kind == '{' {
			closing = '}'
		}
		if current == closing {
			parser.frames = parser.frames[:len(parser.frames)-1]
			parser.offset++
			return nil
		}
		if current != ',' {
			return ErrIntegrity
		}
		frame.state = 1
		parser.offset++
		return nil
	}
	if frame.kind == '[' && frame.state == 0 && current == ']' {
		parser.frames = parser.frames[:len(parser.frames)-1]
		parser.offset++
		return nil
	}
	frame.state = 3
	return parser.value()
}

func canonicalPayloadString(ctx context.Context, raw []byte, start int) (int, error) {
	end, err := canonicalStringEnd(ctx, raw, start)
	if err != nil {
		return 0, err
	}
	for offset := start + 1; offset < end-1; offset++ {
		if (offset-start-1)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		current := raw[offset]
		if current < 0x20 || current >= utf8.RuneSelf || current == '\\' || current == '<' || current == '>' || current == '&' {
			if err := verifyCanonicalStringFragments(ctx, raw[start+1:end-1]); err != nil {
				return 0, err
			}
			break
		}
	}
	return end, ctx.Err()
}

func canonicalPayloadKeysOrdered(ctx context.Context, raw []byte, leftStart, leftEnd, rightStart, rightEnd int) (bool, error) {
	left, right := leftStart+1, rightStart+1
	for compared := 0; ; compared++ {
		if compared%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if left == leftEnd-1 || right == rightEnd-1 {
			return left == leftEnd-1 && right != rightEnd-1, ctx.Err()
		}
		leftRune, leftWidth := canonicalPayloadKeyRune(raw[left:])
		rightRune, rightWidth := canonicalPayloadKeyRune(raw[right:])
		if leftRune != rightRune {
			return leftRune < rightRune, ctx.Err()
		}
		left += leftWidth
		right += rightWidth
	}
}

func canonicalPayloadKeyRune(raw []byte) (rune, int) {
	if raw[0] != '\\' {
		return utf8.DecodeRune(raw)
	}
	switch raw[1] {
	case '"', '\\':
		return rune(raw[1]), 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	default:
		var decoded rune
		for _, digit := range raw[2:6] {
			decoded *= 16
			if digit >= '0' && digit <= '9' {
				decoded += rune(digit - '0')
			} else {
				decoded += rune(digit-'a') + 10
			}
		}
		return decoded, 6
	}
}
