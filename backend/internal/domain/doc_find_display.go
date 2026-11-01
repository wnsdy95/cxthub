package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const MaxDocFindDisplayBytes = 512 << 10
const docFindJSONDisplayBytes = int(unsafe.Sizeof(docFindJSONDisplay{}))

var ErrDocFindDisplayUnavailable = errors.New("display text requires a prepared bounded projection")

type DocFindDisplayText interface {
	Text() string
	SourceRange(offset, length int) (DocFindSpan, error)
}

type docFindDisplayPart struct {
	from, to int
	display  DocFindDisplayText
}

type docFindCompositeDisplay struct {
	text  string
	parts []docFindDisplayPart
}

type docFindJSONDisplay struct {
	display    DocFindDisplayText
	raw        []byte
	sourceFrom int
	source     docFindSourceStringIndex
}

type docFindLiteralDisplay struct {
	text string
	span DocFindSpan
}

type docFindDisplayBlock struct {
	display           DocFindDisplayText
	leading, trailing int
	anchor            DocFindSpan
}

func BuildDocEventDisplay(ctx context.Context, raw []byte) (result DocFindDisplayText, failure error) {
	return BuildDocEventDisplayWithin(ctx, raw, MaxDocFindDisplayBytes)
}

func BuildDocEventDisplayWithin(ctx context.Context, raw []byte, budget int) (result DocFindDisplayText, failure error) {
	defer func() {
		if failure == nil && result != nil && DocFindDisplayBytes(result) > budget {
			result, failure = nil, ErrDocFindDisplayUnavailable
		}
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	if err := validateDocDisplaySource(ctx, raw); err != nil {
		return nil, err
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return nil, ErrIntegrity
	}
	if budget <= 32 || budget > MaxDocFindDisplayBytes {
		return nil, ErrDocFindDisplayUnavailable
	}
	var event struct {
		Kind           EventKind `json:"kind"`
		CompactSummary bool      `json:"compact_summary"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return nil, ErrIntegrity
	}
	if event.Kind == EventToolCall || event.Kind == EventToolResult {
		return buildDocToolDisplay(ctx, raw, budget)
	}
	if event.Kind != EventMessage && event.Kind != EventReasoning {
		return docFindCompositeDisplay{}, nil
	}
	body := bytes.TrimLeft(raw, " \t\r\n")
	key := "blocks"
	if event.Kind == EventReasoning {
		key = "redacted_summary"
	}
	value, from, _ := docRangeJSONFieldPosition(body, key)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return docFindCompositeDisplay{}, nil
	}
	base := len(raw) - len(body) + from
	display, err := buildDocPublicDisplay(ctx, value, event.Kind, event.CompactSummary || event.Kind == EventMessage && docRangeSeedSummary(value), budget-32)
	if err != nil {
		return nil, err
	}
	return docFindShiftedDisplay{display: display, shift: base}, nil
}

func BuildDocPublicDisplay(ctx context.Context, raw []byte, kind EventKind, summary bool) (result DocFindDisplayText, failure error) {
	return BuildDocPublicDisplayWithin(ctx, raw, kind, summary, MaxDocFindDisplayBytes)
}

func BuildDocPublicDisplayWithin(ctx context.Context, raw []byte, kind EventKind, summary bool, budget int) (result DocFindDisplayText, failure error) {
	defer func() {
		if failure == nil && result != nil && DocFindDisplayBytes(result) > budget {
			result, failure = nil, ErrDocFindDisplayUnavailable
		}
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	return buildDocPublicDisplay(ctx, raw, kind, summary, budget)
}

func buildDocPublicDisplay(ctx context.Context, raw []byte, kind EventKind, summary bool, budget int) (DocFindDisplayText, error) {
	if budget <= 0 || budget > MaxDocFindDisplayBytes {
		return nil, ErrDocFindDisplayUnavailable
	}
	if err := validateDocDisplaySource(ctx, raw); err != nil {
		return nil, err
	}
	if kind == EventReasoning {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil, ErrIntegrity
		}
		if strings.TrimFunc(text, docRangePresentationSpace) == "" {
			return docFindCompositeDisplay{}, nil
		}
		block, err := buildDocJSONMarkdown(ctx, raw, 0, budget)
		return block.display, err
	}
	if kind != EventMessage {
		return nil, ErrValidation
	}
	start := len(raw) - len(bytes.TrimLeft(raw, " \t\r\n"))
	if start == len(raw) || raw[start] != '[' {
		return nil, ErrIntegrity
	}
	remaining := raw[start+1:]
	var displays []docFindDisplayBlock
	var summaryParts []docFindDisplayBlock
	used := 128
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining = bytes.TrimLeft(remaining, ", \t\r\n")
		if len(remaining) == 0 {
			return nil, ErrIntegrity
		}
		if remaining[0] == ']' {
			break
		}
		position := len(raw) - len(remaining)
		value, rest := docRangeJSONValue(remaining)
		remaining = rest
		if len(value) == 0 || value[0] != '{' {
			return nil, ErrIntegrity
		}
		var block ContentBlock
		if json.Unmarshal(value, &block) != nil {
			return nil, ErrIntegrity
		}
		if summary || block.Type == "text" {
			text, from, _ := docRangeJSONFieldPosition(value, "text")
			anchor := DocFindSpan{From: position, To: position + len(value)}
			if len(text) == 0 || bytes.Equal(text, []byte("null")) {
				if summary {
					summaryParts = append(summaryParts, docFindDisplayBlock{display: docFindLiteralDisplay{span: anchor}, anchor: anchor})
					used += 128
					if used > budget {
						return nil, ErrDocFindDisplayUnavailable
					}
				}
				continue
			}
			if summary {
				var decoded string
				if json.Unmarshal(text, &decoded) != nil {
					return nil, ErrIntegrity
				}
				identity := docFindIdentityDisplay(decoded)
				source, err := newDocFindSourceStringIndex(ctx, text, budget-used-DocFindDisplayBytes(identity)-docFindJSONDisplayBytes-len(decoded)-80)
				if err != nil {
					return nil, err
				}
				display := docFindJSONDisplay{display: identity, raw: text, sourceFrom: position + from, source: source}
				summaryParts = append(summaryParts, docFindDisplayBlock{display: display, anchor: anchor})
				used += DocFindDisplayBytes(display) + len(decoded) + 80
			} else {
				display, err := buildDocJSONMarkdown(ctx, text, position+from, (budget-used-96)/2)
				if err != nil {
					return nil, err
				}
				displays = append(displays, display)
				used += DocFindDisplayBytes(display.display) + len(display.display.Text()) + 96
			}
		} else {
			text := "[" + block.Type
			var named struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(value, &named) != nil {
				return nil, ErrIntegrity
			}
			if named.Name != "" {
				text += ": " + named.Name
			}
			text += "]"
			displays = append(displays, docFindDisplayBlock{display: docFindLiteralDisplay{text: text, span: DocFindSpan{From: position, To: position + len(value)}}, leading: 2, trailing: 2})
			used += 2*len(text) + 128
		}
		if used > budget {
			return nil, ErrDocFindDisplayUnavailable
		}
	}
	if summary {
		combined, err := joinDocDisplayBlocks(summaryParts, true, budget)
		if err != nil {
			return nil, err
		}
		remaining := budget - DocFindDisplayBytes(combined) - 32
		if remaining <= 0 {
			return nil, ErrDocFindDisplayUnavailable
		}
		markdown, err := buildDocMarkdownDisplay(ctx, combined.Text(), remaining)
		if errors.Is(err, ErrDocMarkdownDisplayLimit) {
			return nil, ErrDocFindDisplayUnavailable
		}
		if err != nil {
			return nil, err
		}
		return docFindNestedDisplay{outer: markdown, inner: combined}, nil
	}
	return joinDocDisplayBlocks(displays, false, budget)
}

func validateDocDisplaySource(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(raw) > MaxDocMarkdownDisplayBytes {
		return ErrDocFindDisplayUnavailable
	}
	if len(raw) == 0 || !utf8.Valid(raw) || !json.Valid(raw) {
		return ErrIntegrity
	}
	return nil
}

func buildDocJSONMarkdown(ctx context.Context, raw []byte, from, budget int) (docFindDisplayBlock, error) {
	if budget <= docFindJSONDisplayBytes {
		return docFindDisplayBlock{}, ErrDocFindDisplayUnavailable
	}
	var text string
	if len(raw) < 2 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
		return docFindDisplayBlock{}, ErrIntegrity
	}
	raw = bytes.TrimRight(raw, " \t\r\n")
	source, err := newDocFindSourceStringIndex(ctx, raw, budget-docFindJSONDisplayBytes-1)
	if err != nil {
		return docFindDisplayBlock{}, err
	}
	display, err := buildDocMarkdownDisplay(ctx, text, budget-docFindJSONDisplayBytes-source.retainedBytes())
	if errors.Is(err, ErrDocMarkdownDisplayLimit) {
		return docFindDisplayBlock{}, ErrDocFindDisplayUnavailable
	}
	if err != nil {
		return docFindDisplayBlock{}, err
	}
	return docFindDisplayBlock{display: docFindJSONDisplay{display: display, raw: raw, sourceFrom: from, source: source}, leading: display.leadingBreaks, trailing: display.trailingBreaks}, nil
}

func joinDocDisplayBlocks(blocks []docFindDisplayBlock, literalNewline bool, budget int) (DocFindDisplayText, error) {
	var text strings.Builder
	var parts []docFindDisplayPart
	breaks := 0
	for index, block := range blocks {
		value := block.display.Text()
		breaks = max(breaks, block.leading)
		if literalNewline && index > 0 {
			anchor := block.anchor
			if value != "" {
				var err error
				anchor, err = block.display.SourceRange(0, runeWidth(value, 0))
				if err != nil {
					return nil, err
				}
			}
			if anchor.From < 0 || anchor.From >= anchor.To {
				return nil, ErrIntegrity
			}
			parts = append(parts, docFindDisplayPart{from: text.Len(), to: text.Len() + 1, display: docFindLiteralDisplay{text: "\n", span: anchor}})
			text.WriteByte('\n')
		} else if !literalNewline && text.Len() > 0 && value != "" && breaks > 0 {
			anchor, err := block.display.SourceRange(0, runeWidth(value, 0))
			if err != nil {
				return nil, err
			}
			separator := strings.Repeat("\n", breaks)
			parts = append(parts, docFindDisplayPart{from: text.Len(), to: text.Len() + len(separator), display: docFindLiteralDisplay{text: separator, span: anchor}})
			text.WriteString(separator)
		}
		if value != "" {
			parts = append(parts, docFindDisplayPart{from: text.Len(), to: text.Len() + len(value), display: block.display})
			text.WriteString(value)
			breaks = 0
		}
		breaks = max(breaks, block.trailing)
		if text.Len() > MaxDocFindDisplayBytes {
			return nil, ErrDocFindDisplayUnavailable
		}
	}
	display := docFindCompositeDisplay{text: text.String(), parts: parts}
	if DocFindDisplayBytes(display) > budget {
		return nil, ErrDocFindDisplayUnavailable
	}
	return display, nil
}

func DocFindDisplayBytes(display DocFindDisplayText) int {
	switch value := display.(type) {
	case interface{ retainedBytes() int }:
		return value.retainedBytes()
	case docFindJSONDisplay:
		return docFindJSONDisplayBytes + value.source.retainedBytes() + DocFindDisplayBytes(value.display)
	case docFindShiftedDisplay:
		return 32 + DocFindDisplayBytes(value.display)
	case docFindNestedDisplay:
		return 32 + DocFindDisplayBytes(value.outer) + DocFindDisplayBytes(value.inner)
	case docFindIdentityDisplay:
		return 16 + len(value)
	case docFindLiteralDisplay:
		return 32 + len(value.text)
	case docFindCompositeDisplay:
		cost := 40 + len(value.text) + cap(value.parts)*32
		for _, part := range value.parts {
			cost += DocFindDisplayBytes(part.display)
		}
		return cost
	default:
		return MaxDocFindDisplayBytes + 1
	}
}

func runeWidth(text string, offset int) int {
	_, width := utf8.DecodeRuneInString(text[offset:])
	return width
}

func (display docFindCompositeDisplay) Text() string { return display.text }

func (display docFindCompositeDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(display.text) || offset > len(display.text)-length || !utf8.RuneStart(display.text[offset]) || offset+length < len(display.text) && !utf8.RuneStart(display.text[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	first := sort.Search(len(display.parts), func(index int) bool { return display.parts[index].to > offset })
	span := DocFindSpan{From: int(^uint(0) >> 1)}
	for index := first; index < len(display.parts) && display.parts[index].from < offset+length; index++ {
		part := display.parts[index]
		from, to := max(offset, part.from)-part.from, min(offset+length, part.to)-part.from
		position, err := part.display.SourceRange(from, to-from)
		if err != nil {
			return DocFindSpan{}, err
		}
		span.From, span.To = min(span.From, position.From), max(span.To, position.To)
	}
	if span.From >= span.To {
		return DocFindSpan{}, ErrIntegrity
	}
	return span, nil
}

func (display docFindJSONDisplay) Text() string { return display.display.Text() }

func (display docFindJSONDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	logical, err := display.display.SourceRange(offset, length)
	if err != nil {
		return DocFindSpan{}, err
	}
	span, err := display.source.sourceRange(display.raw, logical.From, logical.To-logical.From)
	if err != nil {
		return DocFindSpan{}, err
	}
	return DocFindSpan{From: display.sourceFrom + span.From, To: display.sourceFrom + span.To}, nil
}

func (display docFindLiteralDisplay) Text() string { return display.text }
func (display docFindLiteralDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(display.text) || offset > len(display.text)-length || !utf8.RuneStart(display.text[offset]) || offset+length < len(display.text) && !utf8.RuneStart(display.text[offset+length]) || display.span.From >= display.span.To {
		return DocFindSpan{}, ErrValidation
	}
	return display.span, nil
}

type docFindIdentityDisplay string

func (display docFindIdentityDisplay) Text() string { return string(display) }
func (display docFindIdentityDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(display) || offset > len(display)-length || !utf8.RuneStart(display[offset]) || offset+length < len(display) && !utf8.RuneStart(display[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	return DocFindSpan{From: offset, To: offset + length}, nil
}

type docFindShiftedDisplay struct {
	display DocFindDisplayText
	shift   int
}

func (display docFindShiftedDisplay) Text() string { return display.display.Text() }
func (display docFindShiftedDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	span, err := display.display.SourceRange(offset, length)
	if err != nil {
		return DocFindSpan{}, err
	}
	span.From, span.To = span.From+display.shift, span.To+display.shift
	return span, nil
}

type docFindNestedDisplay struct{ outer, inner DocFindDisplayText }

func (display docFindNestedDisplay) Text() string { return display.outer.Text() }
func (display docFindNestedDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	span, err := display.outer.SourceRange(offset, length)
	if err != nil {
		return DocFindSpan{}, err
	}
	return display.inner.SourceRange(span.From, span.To-span.From)
}
