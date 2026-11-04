package domain

import (
	"bytes"
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

type docToolDisplayChild struct {
	docFindSourceValue
	first int
}

func docToolDisplayKey(raw []byte) string {
	raw = raw[1 : len(raw)-1]
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw)
	}
	var decoded strings.Builder
	decoded.Grow(len(raw))
	for len(raw) > 0 {
		character, width := docRangeJSONRune(raw)
		if character == utf8.RuneError && width == 6 && raw[0] == '\\' && raw[1] == 'u' {
			unit, _ := docDisplayHexRune(raw)
			if unit >= 0xd800 && unit <= 0xdfff {
				decoded.WriteByte(byte(0xe0 | unit>>12))
				decoded.WriteByte(byte(0x80 | (unit>>6)&0x3f))
				decoded.WriteByte(byte(0x80 | unit&0x3f))
				raw = raw[width:]
				continue
			}
		}
		decoded.WriteRune(character)
		raw = raw[width:]
	}
	return decoded.String()
}

func (builder *docToolDisplayBuilder) children(parent DocFindSpan) []docToolDisplayChild {
	var children []docToolDisplayChild
	object := builder.parser.raw[parent.From] == '{'
	for position := builder.parser.space(parent.From + 1); position < parent.To-1; {
		child := docToolDisplayChild{docFindSourceValue: builder.parser.value(position), first: position}
		if object {
			keySpan := child.span
			colon := builder.parser.space(keySpan.To)
			child.docFindSourceValue = builder.parser.value(builder.parser.space(colon + 1))
			child.keySpan = keySpan
			child.key = docToolDisplayKey(builder.raw(keySpan))
		}
		children = append(children, child)
		position = builder.parser.space(child.span.To)
		if builder.parser.raw[position] == ',' {
			position = builder.parser.space(position + 1)
		}
	}
	if !object {
		return children
	}
	slices.SortFunc(children, func(left, right docToolDisplayChild) int {
		if order := cmp.Compare(left.key, right.key); order != 0 {
			return order
		}
		return cmp.Compare(left.first, right.first)
	})
	kept := 0
	for position := 0; position < len(children); {
		end := position + 1
		for end < len(children) && children[end].key == children[position].key {
			end++
		}
		selected := children[end-1]
		selected.first = children[position].first
		children[kept] = selected
		kept++
		position = end
	}
	clear(children[kept:])
	children = children[:kept]
	slices.SortFunc(children, func(left, right docToolDisplayChild) int {
		leftIndex, leftOK := docToolDisplayIndex(left.key)
		rightIndex, rightOK := docToolDisplayIndex(right.key)
		if leftOK != rightOK {
			if leftOK {
				return -1
			}
			return 1
		}
		if leftOK {
			return cmp.Compare(leftIndex, rightIndex)
		}
		return cmp.Compare(left.first, right.first)
	})
	return children
}
