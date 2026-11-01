package domain

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const DocToolDisplayVersion = 1
const MaxDocToolDisplayBytes = 512 << 10
const docToolDisplaySpansPerPage = 256
const docToolDisplayLinear = uint32(1 << 31)
const docToolDisplayMaxDepth = 512
const docToolDisplayBaseBytes = int(unsafe.Sizeof(docToolDisplay{}))
const docToolDisplaySpanBytes = int(unsafe.Sizeof(docToolDisplaySpan{}))
const docToolDisplayPageHeaderBytes = int(unsafe.Sizeof([]docToolDisplaySpan{}))

type docToolDisplaySpan struct {
	end        uint32
	sourceFrom uint32
	sourceTo   uint32
}

type docToolDisplay struct {
	text        string
	spans       [][]docToolDisplaySpan
	spanCount   int
	sourceBytes int
}

type docToolDisplayBuilder struct {
	ctx      context.Context
	parser   docFindSourceParser
	text     strings.Builder
	display  docToolDisplay
	metadata int
	budget   int
}

func BuildDocToolDisplay(ctx context.Context, rawEvent []byte) (display DocFindDisplayText, failure error) {
	return buildDocToolDisplay(ctx, rawEvent, MaxDocToolDisplayBytes)
}

func buildDocToolDisplay(ctx context.Context, rawEvent []byte, budget int) (display DocFindDisplayText, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			display, failure = nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget = min(budget, MaxDocToolDisplayBytes)
	if budget < docToolDisplayBaseBytes {
		return nil, ErrDocFindDisplayUnavailable
	}
	if err := validateDocFindSource(rawEvent); err != nil {
		return nil, err
	}
	body := bytes.TrimSpace(rawEvent)
	if body[0] != '{' {
		return nil, ErrIntegrity
	}
	origin := len(rawEvent) - len(bytes.TrimLeft(rawEvent, " \t\r\n"))
	kind := docRangeJSONField(body, "kind")
	if len(kind) == 0 || kind[0] != '"' {
		return nil, ErrIntegrity
	}
	call := docRangeJSONTextEquals(kind, string(EventToolCall))
	if !call && !docRangeJSONTextEquals(kind, string(EventToolResult)) {
		return nil, ErrValidation
	}
	span := DocFindSpan{From: origin, To: origin + len(body)}
	parser, err := newDocFindSourceSyntaxParser(rawEvent, span, false)
	if err != nil {
		return nil, err
	}
	builder := docToolDisplayBuilder{ctx: ctx, parser: parser, display: docToolDisplay{sourceBytes: len(rawEvent)}, metadata: docToolDisplayBaseBytes, budget: budget}
	if call {
		err = builder.call(span)
	} else {
		err = builder.result(span)
	}
	if err != nil {
		return nil, err
	}
	builder.display.text = builder.text.String()
	result := builder.display
	return &result, nil
}

func (display *docToolDisplay) Text() string {
	return display.text
}

func (display *docToolDisplay) retainedBytes() int {
	retained := docToolDisplayBaseBytes + len(display.text) + cap(display.spans)*docToolDisplayPageHeaderBytes
	for _, page := range display.spans {
		retained += cap(page) * docToolDisplaySpanBytes
	}
	return retained
}

func (display *docToolDisplay) span(index int) docToolDisplaySpan {
	return display.spans[index/docToolDisplaySpansPerPage][index%docToolDisplaySpansPerPage]
}

func (display *docToolDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(display.text) || offset > len(display.text)-length ||
		!utf8.RuneStart(display.text[offset]) || offset+length < len(display.text) && !utf8.RuneStart(display.text[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	first := sort.Search(display.spanCount, func(index int) bool { return int(display.span(index).end) > offset })
	from, to := display.sourceBytes, 0
	for index := first; index < display.spanCount; index++ {
		span := display.span(index)
		start := 0
		if index > 0 {
			start = int(display.span(index - 1).end)
		}
		sourceFrom, sourceTo := int(span.sourceFrom), int(span.sourceTo&^docToolDisplayLinear)
		if span.sourceTo&docToolDisplayLinear != 0 {
			sourceFrom += max(offset-start, 0)
			sourceTo -= max(int(span.end)-offset-length, 0)
		}
		from, to = min(from, sourceFrom), max(to, sourceTo)
		if int(span.end) >= offset+length {
			if from < 0 || from >= to || to > display.sourceBytes {
				return DocFindSpan{}, ErrIntegrity
			}
			return DocFindSpan{From: from, To: to}, nil
		}
	}
	return DocFindSpan{}, ErrIntegrity
}

func (builder *docToolDisplayBuilder) emit(text string, source DocFindSpan, linear bool) error {
	if err := builder.ctx.Err(); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if source.From < 0 || source.From >= source.To || source.To > builder.display.sourceBytes ||
		linear && source.To-source.From != len(text) {
		return ErrIntegrity
	}
	if len(text) > builder.budget-builder.text.Len()-builder.metadata {
		return ErrDocFindDisplayUnavailable
	}
	end := uint32(builder.text.Len() + len(text))
	if builder.display.spanCount > 0 {
		page := builder.display.spans[len(builder.display.spans)-1]
		previous := &page[len(page)-1]
		previousLinear := previous.sourceTo&docToolDisplayLinear != 0
		if linear && previousLinear && previous.sourceTo&^docToolDisplayLinear == uint32(source.From) ||
			!linear && !previousLinear && previous.sourceFrom == uint32(source.From) && previous.sourceTo == uint32(source.To) {
			previous.end = end
			if linear {
				previous.sourceTo = uint32(source.To) | docToolDisplayLinear
			}
			builder.text.WriteString(text)
			return nil
		}
	}
	if builder.display.spanCount%docToolDisplaySpansPerPage == 0 {
		capacity := cap(builder.display.spans)
		pageBytes := docToolDisplaySpansPerPage * docToolDisplaySpanBytes
		if len(builder.display.spans) == capacity {
			nextCapacity := max(1, capacity*2)
			pageBytes += (nextCapacity - capacity) * docToolDisplayPageHeaderBytes
			capacity = nextCapacity
		}
		if pageBytes > builder.budget-builder.text.Len()-len(text)-builder.metadata {
			return ErrDocFindDisplayUnavailable
		}
		if capacity > cap(builder.display.spans) {
			pages := make([][]docToolDisplaySpan, len(builder.display.spans), capacity)
			copy(pages, builder.display.spans)
			builder.display.spans = pages
		}
		builder.display.spans = append(builder.display.spans, make([]docToolDisplaySpan, 0, docToolDisplaySpansPerPage))
		builder.metadata += pageBytes
	}
	sourceTo := uint32(source.To)
	if linear {
		sourceTo |= docToolDisplayLinear
	}
	last := len(builder.display.spans) - 1
	builder.display.spans[last] = append(builder.display.spans[last], docToolDisplaySpan{end: end, sourceFrom: uint32(source.From), sourceTo: sourceTo})
	builder.display.spanCount++
	builder.text.WriteString(text)
	return nil
}

func (builder *docToolDisplayBuilder) field(parent DocFindSpan, name string) DocFindSpan {
	if parent.From == parent.To {
		return DocFindSpan{}
	}
	_, span := docFindSourceField(builder.parser.raw[parent.From:parent.To], parent.From, name)
	return span
}

func (builder *docToolDisplayBuilder) raw(span DocFindSpan) []byte {
	return builder.parser.raw[span.From:span.To]
}

func (builder *docToolDisplayBuilder) textValue(span DocFindSpan) string {
	if span.From == span.To || builder.parser.raw[span.From] != '"' {
		return ""
	}
	return docFindSourceKey(builder.raw(span))
}

func (builder *docToolDisplayBuilder) truthy(span DocFindSpan) bool {
	if span.From == span.To {
		return false
	}
	raw := builder.raw(span)
	switch raw[0] {
	case 'n', 'f':
		return false
	case '"':
		return len(raw) > 2
	case '{', '[', 't':
		return true
	default:
		number, err := strconv.ParseFloat(string(raw), 64)
		return (err == nil || errors.Is(err, strconv.ErrRange)) && number != 0
	}
}

func (builder *docToolDisplayBuilder) call(event DocFindSpan) error {
	provider, tool := builder.field(event, "provider_tool_name"), builder.field(event, "tool_name")
	if !docFindSourceOptionalText(builder.raw(provider)) || !docFindSourceOptionalText(builder.raw(tool)) {
		return ErrIntegrity
	}
	nameSpan, name := provider, builder.textValue(provider)
	if name == "" {
		nameSpan, name = tool, builder.textValue(tool)
	}
	if name == "" {
		name = "tool"
		nameSpan = builder.field(event, "kind")
		if err := builder.emit(name, nameSpan, false); err != nil {
			return err
		}
	} else if err := builder.string(nameSpan, false, 0); err != nil {
		return err
	}
	input := builder.field(event, "input")
	if input.From == input.To || bytes.Equal(builder.raw(input), []byte("null")) {
		return nil
	}
	if builder.parser.raw[input.From] != '{' {
		return ErrIntegrity
	}
	filePath := builder.field(input, "file_path")
	headSpan, head := filePath, builder.textValue(filePath)
	if head == "" {
		for _, field := range []string{"command", "pattern", "url"} {
			headSpan = builder.field(input, field)
			head = builder.textValue(headSpan)
			if head != "" {
				break
			}
		}
	}
	if head != "" {
		if err := builder.emit("(", DocFindSpan{From: headSpan.From, To: headSpan.From + 1}, false); err != nil {
			return err
		}
		skip := 0
		if headSpan == filePath && strings.Count(head, "/") > 3 {
			skip = len(head)
			for count := 0; count < 3; count++ {
				skip = strings.LastIndexByte(head[:skip], '/')
			}
			skip++
			if err := builder.emit("…/", DocFindSpan{From: headSpan.From, To: headSpan.From + 1}, false); err != nil {
				return err
			}
		}
		if err := builder.string(headSpan, false, skip); err != nil {
			return err
		}
		if err := builder.emit(")", DocFindSpan{From: headSpan.To - 1, To: headSpan.To}, false); err != nil {
			return err
		}
	}
	old, next := builder.field(input, "old_string"), builder.field(input, "new_string")
	content, command := builder.field(input, "content"), builder.field(input, "command")
	switch {
	case name == "Edit" && (builder.truthy(old) || builder.truthy(next)):
		for position, span := range []DocFindSpan{old, next} {
			if raw := builder.raw(span); len(raw) > 2 && raw[0] == '"' {
				if err := builder.emit("\n", DocFindSpan{From: span.From, To: span.From + 1}, false); err != nil {
					return err
				}
				prefix := "− "
				if position == 1 {
					prefix = "+ "
				}
				if err := builder.diff(span, prefix); err != nil {
					return err
				}
			}
		}
	case name == "Write" && builder.truthy(content):
		if err := builder.emit("\n", DocFindSpan{From: content.From, To: content.From + 1}, false); err != nil {
			return err
		}
		return builder.diff(content, "+ ")
	case name == "Bash" && builder.truthy(command):
		if err := builder.emit("\n$ ", DocFindSpan{From: command.From, To: command.From + 1}, false); err != nil {
			return err
		}
		if raw := builder.raw(command); raw[0] == '"' {
			return builder.string(command, false, 0)
		}
	default:
		if builder.parser.raw[builder.parser.space(input.From+1)] != '}' {
			if err := builder.emit("\n", DocFindSpan{From: input.From, To: input.From + 1}, false); err != nil {
				return err
			}
			return builder.pretty(input, 0)
		}
	}
	return nil
}

func (builder *docToolDisplayBuilder) result(event DocFindSpan) error {
	output := builder.field(event, "output")
	if output.From == output.To || bytes.Equal(builder.raw(output), []byte("null")) {
		return nil
	}
	if builder.parser.raw[output.From] == '"' {
		text := builder.textValue(output)
		if strings.TrimFunc(text, docRangePresentationSpace) == "" {
			return nil
		}
		return builder.string(output, false, 0)
	}
	return builder.pretty(output, 0)
}

func (builder *docToolDisplayBuilder) string(span DocFindSpan, quoted bool, skip int) error {
	if quoted {
		if err := builder.emit("\"", DocFindSpan{From: span.From, To: span.From + 1}, true); err != nil {
			return err
		}
	}
	if err := builder.stringContents(span.From+1, span.To-1, quoted, skip); err != nil {
		return err
	}
	if quoted {
		return builder.emit("\"", DocFindSpan{From: span.To - 1, To: span.To}, true)
	}
	return nil
}

func (builder *docToolDisplayBuilder) stringContents(from, to int, quoted bool, skip int) error {
	for position := from; position < to; {
		if builder.parser.raw[position] != '\\' {
			end := position
			for end < to && builder.parser.raw[end] != '\\' {
				end++
			}
			omitted := min(skip, end-position)
			position, skip = position+omitted, skip-omitted
			if err := builder.emit(string(builder.parser.raw[position:end]), DocFindSpan{From: position, To: end}, true); err != nil {
				return err
			}
			position = end
			continue
		}
		character, width := docRangeJSONRune(builder.parser.raw[position:to])
		if skip > 0 {
			skip -= utf8.RuneLen(character)
			if skip < 0 {
				return ErrIntegrity
			}
			position += width
			continue
		}
		var scratch [12]byte
		var text string
		if quoted {
			var err error
			text, _, _, err = docToolStringRune(builder.parser.raw[position:to], DocToolStringQuoted)
			if err != nil {
				return err
			}
		}
		if text == "" {
			count := utf8.EncodeRune(scratch[:], character)
			text = string(scratch[:count])
		}
		source := DocFindSpan{From: position, To: position + width}
		if err := builder.emit(text, source, bytes.Equal([]byte(text), builder.raw(source))); err != nil {
			return err
		}
		position += width
	}
	return nil
}

func (builder *docToolDisplayBuilder) diff(span DocFindSpan, prefix string) error {
	if err := builder.emit(prefix, DocFindSpan{From: span.From, To: span.From + 1}, false); err != nil {
		return err
	}
	if builder.parser.raw[span.From] != '"' {
		return nil
	}
	start := span.From + 1
	for position := start; position < span.To-1; {
		character, width := docRangeJSONRune(builder.parser.raw[position : span.To-1])
		if character == '\n' {
			if err := builder.stringContents(start, position, false, 0); err != nil {
				return err
			}
			if err := builder.emit("\n"+prefix, DocFindSpan{From: position, To: position + width}, false); err != nil {
				return err
			}
			start = position + width
		}
		position += width
	}
	return builder.stringContents(start, span.To-1, false, 0)
}

func (builder *docToolDisplayBuilder) pretty(span DocFindSpan, depth int) error {
	if err := builder.ctx.Err(); err != nil {
		return err
	}
	if depth > docToolDisplayMaxDepth {
		return ErrDocFindDisplayUnavailable
	}
	raw := builder.raw(span)
	switch raw[0] {
	case '"':
		return builder.string(span, true, 0)
	case '{', '[':
		opening := DocFindSpan{From: span.From, To: span.From + 1}
		closing := DocFindSpan{From: span.To - 1, To: span.To}
		if err := builder.emit(string(raw[:1]), opening, true); err != nil {
			return err
		}
		children := builder.children(span)
		for position, child := range children {
			anchor := opening
			if position > 0 {
				previous := children[position-1]
				anchor = DocFindSpan{From: previous.span.To, To: previous.span.To + 1}
				if builder.parser.raw[anchor.From] != ',' {
					anchor = opening
				}
				if err := builder.emit(",", anchor, false); err != nil {
					return err
				}
			}
			if err := builder.emit("\n"+strings.Repeat("  ", depth+1), anchor, false); err != nil {
				return err
			}
			if raw[0] == '{' {
				if err := builder.string(child.keySpan, true, 0); err != nil {
					return err
				}
				colon := builder.parser.space(child.keySpan.To)
				if err := builder.emit(": ", DocFindSpan{From: colon, To: colon + 1}, false); err != nil {
					return err
				}
			}
			if err := builder.pretty(child.span, depth+1); err != nil {
				return err
			}
		}
		if len(children) > 0 {
			if err := builder.emit("\n"+strings.Repeat("  ", depth), closing, false); err != nil {
				return err
			}
		}
		return builder.emit(string(raw[len(raw)-1:]), closing, true)
	case 't', 'f', 'n':
		return builder.emit(string(raw), span, true)
	default:
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) || math.IsNaN(number) {
			return ErrIntegrity
		}
		rendered := docToolNumberFormat(number)
		return builder.emit(rendered, span, bytes.Equal([]byte(rendered), raw))
	}
}

func docToolDisplayIndex(key string) (uint64, bool) {
	if key == "" || len(key) > 10 || len(key) > 1 && key[0] == '0' {
		return 0, false
	}
	for _, character := range key {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	index, err := strconv.ParseUint(key, 10, 32)
	return index, err == nil && index < math.MaxUint32
}
