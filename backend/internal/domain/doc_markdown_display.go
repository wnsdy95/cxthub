package domain

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const DocMarkdownDisplayVersion = 1
const MaxDocMarkdownDisplayBytes = 128 << 10
const docMarkdownDisplaySpansPerPage = 256

var ErrDocMarkdownDisplayLimit = errors.New("Markdown display projection exceeds the bounded source limit")

type DocMarkdownDisplay struct {
	text           string
	spans          [][]docMarkdownDisplaySpan
	spanCount      int
	sourceBytes    int
	leadingBreaks  int
	trailingBreaks int
}

type docMarkdownDisplaySpan struct {
	from, to             int
	sourceFrom, sourceTo int
}

type docMarkdownDisplayBuilder struct {
	source           string
	text             strings.Builder
	spans            [][]docMarkdownDisplaySpan
	spanCount        int
	pendingBreaks    int
	lastSourceEnd    int
	blockStarted     bool
	pendingSpaceFrom int
	pendingSpaceTo   int
	leadingBreaks    int
	budget           int
	metadata         int
	exceeded         bool
}

type docMarkdownLine struct {
	from, to, next int
}

func BuildDocMarkdownDisplay(ctx context.Context, source string) (DocMarkdownDisplay, error) {
	return buildDocMarkdownDisplay(ctx, source, 0)
}

func buildDocMarkdownDisplay(ctx context.Context, source string, budget int) (DocMarkdownDisplay, error) {
	if err := ctx.Err(); err != nil {
		return DocMarkdownDisplay{}, err
	}
	if len(source) > MaxDocMarkdownDisplayBytes {
		return DocMarkdownDisplay{}, ErrDocMarkdownDisplayLimit
	}
	if !utf8.ValidString(source) {
		return DocMarkdownDisplay{}, ErrIntegrity
	}
	builder := docMarkdownDisplayBuilder{source: source, pendingSpaceFrom: -1, budget: budget}
	for position := 0; position <= len(source); {
		if builder.exceeded {
			return DocMarkdownDisplay{}, ErrDocMarkdownDisplayLimit
		}
		if err := ctx.Err(); err != nil {
			return DocMarkdownDisplay{}, err
		}
		line := docMarkdownReadLine(source, position)
		if docMarkdownFence(source[line.from:line.to]) {
			position = line.next
			from, to := position, position
			for position <= len(source) {
				body := docMarkdownReadLine(source, position)
				if docMarkdownFence(source[body.from:body.to]) {
					position = body.next
					break
				}
				to = body.to
				position = body.next
			}
			builder.beginBlock(1)
			if from <= len(source) {
				builder.preserve(from, to)
			}
			builder.endBlock(1)
			continue
		}
		if from, to, ok := docMarkdownHeading(source[line.from:line.to]); ok {
			builder.beginBlock(1)
			builder.inline(line.from+from, line.from+to, true)
			builder.endBlock(1)
			position = line.next
			continue
		}
		if from, ordered, ok := docMarkdownList(source[line.from:line.to]); ok {
			for {
				builder.beginBlock(1)
				builder.inline(line.from+from, line.to, true)
				builder.endBlock(1)
				position = line.next
				if position > len(source) {
					break
				}
				line = docMarkdownReadLine(source, position)
				nextFrom, nextOrdered, nextList := docMarkdownList(source[line.from:line.to])
				if !nextList || nextOrdered != ordered {
					break
				}
				from = nextFrom
			}
			continue
		}
		if docMarkdownTrimStart(source[line.from:line.to]) == line.to-line.from {
			position = line.next
			continue
		}
		from, to := line.from, line.to
		position = line.next
		for position <= len(source) {
			line = docMarkdownReadLine(source, position)
			text := source[line.from:line.to]
			_, _, heading := docMarkdownHeading(text)
			_, _, list := docMarkdownList(text)
			if docMarkdownTrimStart(text) == len(text) || docMarkdownFence(text) || heading || list {
				break
			}
			to, position = line.to, line.next
		}
		builder.beginBlock(2)
		builder.inline(from, to, false)
		builder.endBlock(2)
	}
	if err := ctx.Err(); err != nil {
		return DocMarkdownDisplay{}, err
	}
	if builder.exceeded {
		return DocMarkdownDisplay{}, ErrDocMarkdownDisplayLimit
	}
	return DocMarkdownDisplay{text: builder.text.String(), spans: builder.spans, spanCount: builder.spanCount, sourceBytes: len(source), leadingBreaks: builder.leadingBreaks, trailingBreaks: builder.pendingBreaks}, nil
}

func (display DocMarkdownDisplay) Text() string {
	return display.text
}

func (display DocMarkdownDisplay) retainedBytes() int {
	metadata := cap(display.spans)*24 + 80
	for _, page := range display.spans {
		metadata += cap(page) * 32
	}
	return len(display.text) + metadata
}

func (display DocMarkdownDisplay) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(display.text) || offset > len(display.text)-length ||
		!utf8.RuneStart(display.text[offset]) || offset+length < len(display.text) && !utf8.RuneStart(display.text[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	first := sort.Search(display.spanCount, func(index int) bool { return display.span(index).to > offset })
	last := sort.Search(display.spanCount, func(index int) bool { return display.span(index).to >= offset+length })
	if first >= display.spanCount || last >= display.spanCount {
		return DocFindSpan{}, ErrIntegrity
	}
	start, end := display.span(first), display.span(last)
	from, to := start.sourceFrom, end.sourceTo
	if start.to-start.from == start.sourceTo-start.sourceFrom {
		from += offset - start.from
	}
	if end.to-end.from == end.sourceTo-end.sourceFrom {
		to = end.sourceFrom + offset + length - end.from
	}
	if from < 0 || from >= to || to > display.sourceBytes {
		return DocFindSpan{}, ErrIntegrity
	}
	return DocFindSpan{From: from, To: to}, nil
}

func (display DocMarkdownDisplay) span(index int) docMarkdownDisplaySpan {
	return display.spans[index/docMarkdownDisplaySpansPerPage][index%docMarkdownDisplaySpansPerPage]
}

func docMarkdownReadLine(source string, from int) docMarkdownLine {
	end := strings.IndexByte(source[from:], '\n')
	if end < 0 {
		return docMarkdownLine{from: from, to: len(source), next: len(source) + 1}
	}
	return docMarkdownLine{from: from, to: from + end, next: from + end + 1}
}

func docMarkdownWhitespace(character rune) bool {
	return character >= '\t' && character <= '\r' || character == ' ' || character == '\u00a0' || character == '\u1680' ||
		character >= '\u2000' && character <= '\u200a' || character == '\u2028' || character == '\u2029' ||
		character == '\u202f' || character == '\u205f' || character == '\u3000' || character == '\ufeff'
}

func docMarkdownTrimStart(text string) int {
	for position, character := range text {
		if !docMarkdownWhitespace(character) {
			return position
		}
	}
	return len(text)
}

func docMarkdownFence(text string) bool {
	return strings.HasPrefix(text[docMarkdownTrimStart(text):], "```")
}

func docMarkdownHeading(text string) (int, int, bool) {
	position := 0
	for position < len(text) && text[position] == '#' {
		position++
	}
	if position == 0 || position > 3 || position == len(text) {
		return 0, 0, false
	}
	whitespace := docMarkdownTrimStart(text[position:])
	if whitespace == 0 {
		return 0, 0, false
	}
	position += whitespace
	end := len(text)
	for _, character := range text[position:end] {
		if character == '\r' || character == '\n' || character == '\u2028' || character == '\u2029' {
			return 0, 0, false
		}
	}
	return position, end, true
}

func docMarkdownList(text string) (int, bool, bool) {
	position := docMarkdownTrimStart(text)
	if position == len(text) {
		return 0, false, false
	}
	ordered := text[position] != '-' && text[position] != '*'
	if ordered {
		start := position
		for position < len(text) && text[position] >= '0' && text[position] <= '9' {
			position++
		}
		if position == start || position == len(text) || text[position] != '.' && text[position] != ')' {
			return 0, false, false
		}
	}
	position++
	whitespace := docMarkdownTrimStart(text[position:])
	if whitespace == 0 {
		return 0, false, false
	}
	return position + whitespace, ordered, true
}

func (builder *docMarkdownDisplayBuilder) beginBlock(breaks int) {
	builder.blockStarted = false
	builder.pendingSpaceFrom = -1
	builder.pendingBreaks = max(builder.pendingBreaks, breaks)
}

func (builder *docMarkdownDisplayBuilder) endBlock(breaks int) {
	builder.pendingSpaceFrom = -1
	builder.pendingBreaks = max(builder.pendingBreaks, breaks)
}

func (builder *docMarkdownDisplayBuilder) write(text string, from, to int) {
	if text == "" || builder.exceeded {
		return
	}
	if builder.budget > 0 && builder.text.Len()+len(text)+builder.metadata+80 > builder.budget {
		builder.exceeded = true
		return
	}
	start := builder.text.Len()
	if len(builder.spans) > 0 {
		page := builder.spans[len(builder.spans)-1]
		last := &page[len(page)-1]
		if last.to == start && last.sourceTo == from && last.to-last.from == last.sourceTo-last.sourceFrom && len(text) == to-from {
			builder.text.WriteString(text)
			last.to, last.sourceTo = builder.text.Len(), to
			return
		}
	}
	if builder.spanCount%docMarkdownDisplaySpansPerPage == 0 {
		capacity := min(docMarkdownDisplaySpansPerPage, len(builder.source))
		if builder.budget > 0 && builder.text.Len()+len(text)+builder.metadata+capacity*32+48+80 > builder.budget {
			builder.exceeded = true
			return
		}
		builder.spans = append(builder.spans, make([]docMarkdownDisplaySpan, 0, capacity))
		builder.metadata += capacity*32 + 48
	}
	builder.text.WriteString(text)
	page := len(builder.spans) - 1
	builder.spans[page] = append(builder.spans[page], docMarkdownDisplaySpan{from: start, to: builder.text.Len(), sourceFrom: from, sourceTo: to})
	builder.spanCount++
}

func (builder *docMarkdownDisplayBuilder) content(text string, from, to int) {
	if text == "" {
		return
	}
	if !builder.blockStarted {
		if builder.text.Len() > 0 {
			builder.write(strings.Repeat("\n", builder.pendingBreaks), builder.lastSourceEnd, from)
		} else {
			builder.leadingBreaks = builder.pendingBreaks
		}
		builder.blockStarted, builder.pendingBreaks = true, 0
	}
	builder.write(text, from, to)
	builder.lastSourceEnd = to
}

func (builder *docMarkdownDisplayBuilder) preserve(from, to int) {
	builder.content(builder.source[from:to], from, to)
}

func (builder *docMarkdownDisplayBuilder) collapse(from, to int) {
	for position := from; position < to; {
		character, width := utf8.DecodeRuneInString(builder.source[position:to])
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			if builder.blockStarted {
				if builder.pendingSpaceFrom < 0 {
					builder.pendingSpaceFrom = position
				}
				builder.pendingSpaceTo = position + width
			}
		} else {
			if builder.pendingSpaceFrom >= 0 {
				builder.content(" ", builder.pendingSpaceFrom, builder.pendingSpaceTo)
				builder.pendingSpaceFrom = -1
			}
			builder.content(builder.source[position:position+width], position, position+width)
		}
		position += width
	}
}

func (builder *docMarkdownDisplayBuilder) inline(from, to int, collapse bool) {
	copyText := builder.preserve
	if collapse {
		copyText = builder.collapse
	}
	if strings.IndexAny(builder.source[from:to], "*`[") < 0 {
		copyText(from, to)
		return
	}
	source := builder.source
	last, cursor := from, from
	nextStar, nextBacktick, nextBracket, nextTerminator := -1, -1, -1, -1
	lookahead := func(marker byte, start int) int {
		position := strings.IndexByte(source[start:to], marker)
		if position < 0 {
			return to
		}
		return start + position
	}
	for cursor < to {
		contentFrom, contentTo, end := cursor, cursor, cursor
		switch {
		case source[cursor] == '*' && cursor+1 < to && source[cursor+1] == '*':
			contentFrom = cursor + 2
			if nextStar < contentFrom {
				nextStar = lookahead('*', contentFrom)
			}
			if nextStar > contentFrom && nextStar+1 < to && source[nextStar+1] == '*' {
				contentTo, end = nextStar, nextStar+2
			}
		case source[cursor] == '`':
			contentFrom = cursor + 1
			if nextBacktick < contentFrom {
				nextBacktick = lookahead('`', contentFrom)
			}
			if nextBacktick > contentFrom && nextBacktick < to {
				contentTo, end = nextBacktick, nextBacktick+1
			}
		case source[cursor] == '[':
			contentFrom = cursor + 1
			if nextBracket < contentFrom {
				nextBracket = lookahead(']', contentFrom)
			}
			if nextBracket > contentFrom && nextBracket+1 < to && source[nextBracket+1] == '(' {
				hrefStart, hrefBody := nextBracket+2, -1
				if strings.HasPrefix(source[hrefStart:to], "https://") {
					hrefBody = hrefStart + 8
				} else if strings.HasPrefix(source[hrefStart:to], "http://") {
					hrefBody = hrefStart + 7
				}
				if hrefBody >= 0 {
					if nextTerminator < hrefBody {
						nextTerminator = hrefBody
						for nextTerminator < to {
							character, width := utf8.DecodeRuneInString(source[nextTerminator:to])
							if character == ')' || docMarkdownWhitespace(character) {
								break
							}
							nextTerminator += width
						}
					}
					if nextTerminator > hrefBody && nextTerminator < to && source[nextTerminator] == ')' {
						contentTo, end = nextBracket, nextTerminator+1
					}
				}
			}
		}
		if end > cursor {
			copyText(last, cursor)
			copyText(contentFrom, contentTo)
			cursor, last = end, end
		} else {
			cursor++
		}
	}
	copyText(last, to)
}
