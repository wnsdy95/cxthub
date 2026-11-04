package domain

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxDocFindSourceBytes = 128 << 10
const docFindSourceCheckpointStride = 256
const docFindSourceCheckpointBytes = 8

type docFindSourceCheckpoint struct {
	raw, decoded int32
}

type docFindSourceStringIndex []docFindSourceCheckpoint

func newDocFindSourceStringIndex(ctx context.Context, raw []byte, budget int) (docFindSourceStringIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) < 2 || len(raw) > maxDocFindSourceBytes || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, ErrIntegrity
	}
	capacity := (len(raw)-2)/docFindSourceCheckpointStride + 1
	if capacity*docFindSourceCheckpointBytes > budget {
		return nil, ErrDocFindDisplayUnavailable
	}
	checkpoints := make(docFindSourceStringIndex, 0, capacity)
	decoded, previous := 0, 1
	for position := 1; position < len(raw)-1; {
		if position-previous >= docFindSourceCheckpointStride {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			checkpoints = append(checkpoints, docFindSourceCheckpoint{raw: int32(position), decoded: int32(decoded)})
			previous = position
		}
		character, width := docRangeJSONRune(raw[position : len(raw)-1])
		decoded += utf8.RuneLen(character)
		position += width
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append(checkpoints, docFindSourceCheckpoint{raw: int32(len(raw) - 1), decoded: int32(decoded)}), nil
}

func (index docFindSourceStringIndex) retainedBytes() int {
	return cap(index) * docFindSourceCheckpointBytes
}

func (index docFindSourceStringIndex) sourceRange(raw []byte, offset, length int) (DocFindSpan, error) {
	if len(index) == 0 {
		return DocFindSpan{}, ErrIntegrity
	}
	decodedBytes := int(index[len(index)-1].decoded)
	if offset < 0 || length <= 0 || length > decodedBytes || offset > decodedBytes-length {
		return DocFindSpan{}, ErrValidation
	}
	from, err := index.position(raw, offset)
	if err != nil {
		return DocFindSpan{}, err
	}
	to, err := index.position(raw, offset+length)
	if err != nil {
		return DocFindSpan{}, err
	}
	return DocFindSpan{From: from, To: to}, nil
}

func (index docFindSourceStringIndex) position(raw []byte, offset int) (int, error) {
	next, found := slices.BinarySearchFunc(index, offset, func(checkpoint docFindSourceCheckpoint, target int) int {
		return cmp.Compare(int(checkpoint.decoded), target)
	})
	if found {
		return int(index[next].raw), nil
	}
	decoded, position := 0, 1
	if next > 0 {
		decoded, position = int(index[next-1].decoded), int(index[next-1].raw)
	}
	for decoded < offset {
		character, width := docRangeJSONRune(raw[position : len(raw)-1])
		decoded += utf8.RuneLen(character)
		position += width
	}
	if decoded != offset {
		return 0, ErrValidation
	}
	return position, nil
}

type docFindSourceCursor struct {
	raw      []byte
	requests []docFindSourceRequest
	spans    []DocFindSpan
	field    int
	next     int
	found    bool
	failure  error
}

type docFindSourceRequest struct {
	field  int
	offset int
	length int
	result int
}

type docFindSourceEndpoint struct {
	offset int
	result int
	end    bool
}

type docFindSourceValue struct {
	span    DocFindSpan
	key     string
	keySpan DocFindSpan
}

type docFindSourceParser struct {
	raw  []byte
	base int
	ends []int32
}

func LocateDocFindTextSource(rawEvent []byte, field, offset, length int) (DocFindSpan, error) {
	matches := [1]DocFindMatch{{Field: field, Offset: offset, Length: length}}
	var spans [1]DocFindSpan
	if err := locateDocFindSources(rawEvent, "", false, matches[:], spans[:]); err != nil {
		return DocFindSpan{}, err
	}
	return spans[0], nil
}

func LocateDocFindPublicTextSource(rawPublic []byte, kind EventKind, field, offset, length int) (DocFindSpan, error) {
	matches := [1]DocFindMatch{{Field: field, Offset: offset, Length: length}}
	var spans [1]DocFindSpan
	if err := locateDocFindSources(rawPublic, kind, true, matches[:], spans[:]); err != nil {
		return DocFindSpan{}, err
	}
	return spans[0], nil
}

func LocateDocFindTextSources(raw []byte, matches []DocFindMatch) ([]DocFindSpan, error) {
	if len(matches) > DocFindPageMatches {
		return nil, ErrValidation
	}
	spans := make([]DocFindSpan, len(matches))
	if err := locateDocFindSources(raw, "", false, matches, spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func LocateDocFindPublicTextSources(raw []byte, kind EventKind, matches []DocFindMatch) ([]DocFindSpan, error) {
	if len(matches) > DocFindPageMatches {
		return nil, ErrValidation
	}
	spans := make([]DocFindSpan, len(matches))
	if err := locateDocFindSources(raw, kind, true, matches, spans); err != nil {
		return nil, err
	}
	return spans, nil
}

func locateDocFindSources(raw []byte, kind EventKind, public bool, matches []DocFindMatch, spans []DocFindSpan) error {
	var requests [DocFindPageMatches]docFindSourceRequest
	for position, match := range matches {
		if match.Field < 0 || match.Offset < 0 || match.Length <= 0 || match.Offset > int(^uint(0)>>1)-match.Length {
			return ErrValidation
		}
		requests[position] = docFindSourceRequest{field: match.Field, offset: match.Offset, length: match.Length, result: position}
	}
	if err := validateDocFindSource(raw); err != nil {
		return err
	}
	ordered := requests[:len(matches)]
	slices.SortFunc(ordered, func(left, right docFindSourceRequest) int { return cmp.Compare(left.field, right.field) })
	cursor := docFindSourceCursor{raw: raw, requests: ordered, spans: spans, found: len(matches) == 0}
	if public {
		if err := cursor.public(kind); err != nil {
			return err
		}
	} else if err := cursor.event(); err != nil {
		return err
	}
	if cursor.failure != nil {
		if cursor.failure == ErrIntegrity {
			return ErrIntegrity
		}
		return ErrValidation
	}
	if !cursor.found {
		return ErrValidation
	}
	return nil
}

func (cursor *docFindSourceCursor) event() error {
	rawEvent := cursor.raw
	body := bytes.TrimLeft(rawEvent, " \t\r\n")
	if body[0] != '{' {
		return ErrIntegrity
	}
	origin := len(rawEvent) - len(body)
	kind := docRangeJSONField(body, "kind")
	switch {
	case docRangeJSONTextEquals(kind, string(EventMessage)), docRangeJSONTextEquals(kind, string(EventTurn)):
		blocks, span := docFindSourceField(body, origin, "blocks")
		if err := cursor.blocks(blocks, span.From); err != nil {
			return err
		}
	case docRangeJSONTextEquals(kind, string(EventReasoning)):
		text, span := docFindSourceField(body, origin, "redacted_summary")
		if err := cursor.text(text, span); err != nil {
			return err
		}
	case docRangeJSONTextEquals(kind, string(EventToolCall)):
		provider, providerSpan := docFindSourceField(body, origin, "provider_tool_name")
		tool, toolSpan := docFindSourceField(body, origin, "tool_name")
		if !docFindSourceOptionalText(provider) || !docFindSourceOptionalText(tool) {
			return ErrIntegrity
		}
		if len(provider) > 2 && provider[0] == '"' {
			cursor.add(providerSpan)
		} else if err := cursor.text(tool, toolSpan); err != nil {
			return err
		}
		input, span := docFindSourceField(body, origin, "input")
		if len(input) != 0 && !bytes.Equal(input, []byte("null")) {
			if input[0] != '{' {
				return ErrIntegrity
			}
			parser, err := newDocFindSourceParser(rawEvent, span)
			if err != nil {
				return err
			}
			cursor.visit(&parser, docFindSourceValue{span: span})
		}
	case docRangeJSONTextEquals(kind, string(EventToolResult)):
		output, span := docFindSourceField(body, origin, "output")
		if len(output) != 0 && !bytes.Equal(output, []byte("null")) {
			parser, err := newDocFindSourceParser(rawEvent, span)
			if err != nil {
				return err
			}
			cursor.visit(&parser, docFindSourceValue{span: span})
		}
	default:
		return ErrValidation
	}
	return nil
}

func (cursor *docFindSourceCursor) public(kind EventKind) error {
	rawPublic := cursor.raw
	body := bytes.TrimSpace(rawPublic)
	origin := len(rawPublic) - len(bytes.TrimLeft(rawPublic, " \t\r\n"))
	switch kind {
	case EventMessage, EventTurn:
		if body[0] != '[' {
			return ErrIntegrity
		}
		if err := cursor.blocks(body, origin); err != nil {
			return err
		}
	case EventReasoning:
		if body[0] != '"' {
			return ErrIntegrity
		}
		if err := cursor.text(body, DocFindSpan{From: origin, To: origin + len(body)}); err != nil {
			return err
		}
	default:
		return ErrValidation
	}
	return nil
}

func validateDocFindSource(raw []byte) error {
	if len(raw) == 0 || len(raw) > maxDocFindSourceBytes || !utf8.Valid(raw) || !json.Valid(raw) {
		return ErrIntegrity
	}
	return nil
}

func docFindSourceField(body []byte, origin int, name string) ([]byte, DocFindSpan) {
	value, from, to := docRangeJSONFieldPosition(body, name)
	return value, DocFindSpan{From: origin + from, To: origin + to}
}

func docFindSourceOptionalText(raw []byte) bool {
	return len(raw) == 0 || raw[0] == '"' || bytes.Equal(raw, []byte("null"))
}

func (cursor *docFindSourceCursor) add(span DocFindSpan) {
	if cursor.found {
		return
	}
	if cursor.field == cursor.requests[cursor.next].field {
		end := cursor.next + 1
		for end < len(cursor.requests) && cursor.requests[end].field == cursor.field {
			end++
		}
		cursor.failure = cursor.locate(span, cursor.requests[cursor.next:end])
		cursor.next = end
		cursor.found = end == len(cursor.requests) || cursor.failure != nil
	}
	cursor.field++
}

func (cursor *docFindSourceCursor) text(raw []byte, span DocFindSpan) error {
	if !docFindSourceOptionalText(raw) {
		return ErrIntegrity
	}
	if len(raw) > 2 && raw[0] == '"' {
		cursor.add(span)
	}
	return nil
}

func (cursor *docFindSourceCursor) blocks(raw []byte, origin int) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] != '[' {
		return ErrIntegrity
	}
	remaining := raw[1:]
	for {
		remaining = bytes.TrimLeft(remaining, ", \t\r\n")
		if remaining[0] == ']' {
			return nil
		}
		blockOrigin := origin + len(raw) - len(remaining)
		block, rest := docRangeJSONValue(remaining)
		remaining = rest
		if bytes.Equal(block, []byte("null")) {
			continue
		}
		if block[0] != '{' {
			return ErrIntegrity
		}
		kind := docRangeJSONField(block, "type")
		text, span := docFindSourceField(block, blockOrigin, "text")
		if !docFindSourceOptionalText(kind) || !docFindSourceOptionalText(text) {
			return ErrIntegrity
		}
		if docRangeJSONTextEquals(kind, "text") {
			if err := cursor.text(text, span); err != nil {
				return err
			}
		}
	}
}

func (parser *docFindSourceParser) space(position int) int {
	for position < len(parser.raw) {
		switch parser.raw[position] {
		case ' ', '\t', '\r', '\n':
			position++
		default:
			return position
		}
	}
	return position
}

func docFindSourceKey(raw []byte) string {
	raw = raw[1 : len(raw)-1]
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw)
	}
	var decoded strings.Builder
	decoded.Grow(len(raw))
	for len(raw) > 0 {
		character, width := docRangeJSONRune(raw)
		decoded.WriteRune(character)
		raw = raw[width:]
	}
	return decoded.String()
}

func newDocFindSourceParser(raw []byte, span DocFindSpan) (docFindSourceParser, error) {
	return newDocFindSourceSyntaxParser(raw, span, true)
}

func newDocFindSourceSyntaxParser(raw []byte, span DocFindSpan, finiteNumbers bool) (docFindSourceParser, error) {
	parser := docFindSourceParser{raw: raw, base: span.From}
	if raw[span.From] == '{' || raw[span.From] == '[' {
		parser.ends = make([]int32, span.To-span.From)
	}
	var initial [32]int
	stack := initial[:0]
	for position := span.From; position < span.To; {
		switch raw[position] {
		case '{', '[':
			stack = append(stack, position)
			position++
		case '}', ']':
			opening := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			parser.ends[opening-parser.base] = int32(position + 1)
			position++
		case ':', ',', ' ', '\t', '\r', '\n':
			position++
		default:
			value, _ := docRangeJSONValue(raw[position:span.To])
			if finiteNumbers && raw[position] != '"' && raw[position] != 't' && raw[position] != 'f' && raw[position] != 'n' {
				if _, err := strconv.ParseFloat(string(value), 64); err != nil {
					return docFindSourceParser{}, ErrIntegrity
				}
			}
			if parser.ends != nil {
				parser.ends[position-parser.base] = int32(position + len(value))
			}
			position += len(value)
		}
	}
	return parser, nil
}

func (parser *docFindSourceParser) value(position int) docFindSourceValue {
	return docFindSourceValue{span: DocFindSpan{From: position, To: int(parser.ends[position-parser.base])}}
}

func (cursor *docFindSourceCursor) visit(parser *docFindSourceParser, value docFindSourceValue) {
	if cursor.found {
		return
	}
	switch parser.raw[value.span.From] {
	case '[':
		for position := parser.space(value.span.From + 1); parser.raw[position] != ']'; {
			child := parser.value(position)
			cursor.visit(parser, child)
			if cursor.found {
				return
			}
			position = parser.space(child.span.To)
			if parser.raw[position] == ',' {
				position = parser.space(position + 1)
			}
		}
	case '{':
		var children []docFindSourceValue
		for position := parser.space(value.span.From + 1); parser.raw[position] != '}'; {
			keySpan := parser.value(position).span
			position = parser.space(keySpan.To)
			child := parser.value(parser.space(position + 1))
			child.key = docFindSourceKey(parser.raw[keySpan.From:keySpan.To])
			child.keySpan = keySpan
			children = append(children, child)
			position = parser.space(child.span.To)
			if parser.raw[position] == ',' {
				position = parser.space(position + 1)
			}
		}
		slices.SortFunc(children, func(left, right docFindSourceValue) int {
			if order := cmp.Compare(left.key, right.key); order != 0 {
				return order
			}
			return cmp.Compare(right.keySpan.From, left.keySpan.From)
		})
		for position, child := range children {
			if position > 0 && children[position-1].key == child.key {
				continue
			}
			if child.key != "" {
				cursor.add(child.keySpan)
			}
			cursor.visit(parser, child)
			if cursor.found {
				return
			}
		}
	case '"':
		if value.span.To-value.span.From > 2 {
			cursor.add(value.span)
		}
	default:
		cursor.add(value.span)
	}
}

func (cursor *docFindSourceCursor) locate(span DocFindSpan, requests []docFindSourceRequest) error {
	raw := cursor.raw
	value := raw[span.From:span.To]
	if value[0] != '"' {
		normalized := value
		if value[0] != 't' && value[0] != 'f' && value[0] != 'n' {
			number, err := strconv.ParseFloat(string(value), 64)
			if err != nil {
				return ErrIntegrity
			}
			normalized, err = json.Marshal(number)
			if err != nil {
				return ErrIntegrity
			}
		}
		for _, request := range requests {
			if request.length > len(normalized) || request.offset > len(normalized)-request.length {
				return ErrValidation
			}
			cursor.spans[request.result] = span
			if bytes.Equal(normalized, value) {
				cursor.spans[request.result] = DocFindSpan{From: span.From + request.offset, To: span.From + request.offset + request.length}
			}
		}
		return nil
	}
	var endpoints [DocFindPageMatches * 2]docFindSourceEndpoint
	for position, request := range requests {
		endpoints[position*2] = docFindSourceEndpoint{offset: request.offset, result: request.result}
		endpoints[position*2+1] = docFindSourceEndpoint{offset: request.offset + request.length, result: request.result, end: true}
	}
	ordered := endpoints[:len(requests)*2]
	slices.SortFunc(ordered, func(left, right docFindSourceEndpoint) int { return cmp.Compare(left.offset, right.offset) })
	decoded, next, position := 0, 0, span.From+1
	for next < len(ordered) {
		for next < len(ordered) && ordered[next].offset == decoded {
			endpoint := ordered[next]
			if endpoint.end {
				cursor.spans[endpoint.result].To = position
			} else {
				cursor.spans[endpoint.result].From = position
			}
			next++
		}
		if next == len(ordered) {
			return nil
		}
		if position == span.To-1 {
			return ErrValidation
		}
		character, width := docRangeJSONRune(raw[position : span.To-1])
		decoded += utf8.RuneLen(character)
		position += width
		if decoded > ordered[next].offset {
			return ErrValidation
		}
	}
	return nil
}
