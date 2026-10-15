package domain

import (
	"context"
	"unicode/utf8"
)

type legacyStreamScanner struct {
	stack                      [9998]byte
	depth, start, offset       int
	quoted, escaped, needComma bool
	afterComma, havePrevious   bool
	previous                   int
	event                      []byte
	limit                      int
	visit                      func(canonicalSpan, []byte) (int, error)
}

func (scanner *legacyStreamScanner) add(ctx context.Context, body []byte) error {
	for _, char := range body {
		if scanner.offset%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if scanner.depth == 0 {
			if scanner.needComma {
				if char != ',' {
					return ErrIntegrity
				}
				scanner.needComma, scanner.afterComma = false, true
				scanner.offset++
				continue
			}
			if char != '{' {
				return ErrIntegrity
			}
			scanner.start = scanner.offset
			scanner.event = scanner.event[:0]
			scanner.afterComma = false
		}
		if len(scanner.event) >= scanner.limit {
			return ErrDocumentResourceLimit
		}
		if len(scanner.event) == cap(scanner.event) {
			grown := make([]byte, len(scanner.event), min(scanner.limit, max(1024, cap(scanner.event)*2)))
			copy(grown, scanner.event)
			scanner.event = grown
		}
		scanner.event = append(scanner.event, char)
		if scanner.quoted {
			if scanner.escaped {
				scanner.escaped = false
			} else if char == '\\' {
				scanner.escaped = true
			} else if char == '"' {
				scanner.quoted = false
			}
		} else {
			switch char {
			case '"':
				scanner.quoted = true
			case '{', '[':
				if scanner.depth == len(scanner.stack) {
					return ErrIntegrity
				}
				scanner.stack[scanner.depth] = char
				scanner.depth++
			case '}', ']':
				if scanner.depth == 0 || (char == '}' && scanner.stack[scanner.depth-1] != '{') || (char == ']' && scanner.stack[scanner.depth-1] != '[') {
					return ErrIntegrity
				}
				scanner.depth--
				if scanner.depth == 0 {
					if !utf8.Valid(scanner.event) {
						return ErrIntegrity
					}
					sequence, err := scanner.visit(canonicalSpan{offset: scanner.start, length: len(scanner.event)}, scanner.event)
					if err != nil {
						return err
					}
					if scanner.havePrevious && sequence < scanner.previous {
						return ErrIntegrity
					}
					scanner.previous, scanner.havePrevious, scanner.needComma = sequence, true, true
				}
			case ' ', '\n', '\r', '\t':
				return ErrIntegrity
			}
		}
		scanner.offset++
	}
	return ctx.Err()
}

func (scanner *legacyStreamScanner) finish(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanner.depth != 0 || scanner.quoted || scanner.afterComma {
		return ErrIntegrity
	}
	return nil
}
