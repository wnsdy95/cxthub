package domain

import (
	"context"
	"strings"
	"unicode/utf8"
)

type DocDisplaySearchWindow struct {
	from     int
	boundary int
	display  DocFindDisplayText
}

func (window DocDisplaySearchWindow) From() int { return window.from }
func (window DocDisplaySearchWindow) To() int {
	if window.display == nil {
		return window.from
	}
	return window.from + len(window.display.Text())
}
func (window DocDisplaySearchWindow) Text() string {
	if window.display == nil {
		return ""
	}
	return window.display.Text()
}

type docDisplayPageSlice struct {
	page  DocDisplayPage
	start int
}

func (display docDisplayPageSlice) Text() string { return display.page.Text[display.start:] }
func (display docDisplayPageSlice) SourceRange(offset, length int) (DocFindSpan, error) {
	return display.page.SourceRange(display.start+offset, length)
}

func BuildDocDisplaySearchWindow(source DocDisplayStringSource, previous *DocDisplayPage, current DocDisplayPage) (DocDisplaySearchWindow, error) {
	if current.Validate(source) != nil || (current.Index == 0) != (previous == nil) {
		return DocDisplaySearchWindow{}, ErrIntegrity
	}
	if previous != nil && (previous.Validate(source) != nil || previous.Runs[len(previous.Runs)-1].SourceTo != current.Runs[0].SourceFrom) {
		return DocDisplaySearchWindow{}, ErrIntegrity
	}
	return buildDocDisplaySearchWindow(previous, current)
}

func BuildDocDisplayProjectionSearchWindow(source DocDisplayProjectionSource, previous *DocDisplayPage, current DocDisplayPage) (DocDisplaySearchWindow, error) {
	if current.ValidateProjection(source) != nil || (current.Index == 0) != (previous == nil) || previous != nil && previous.ValidateProjection(source) != nil {
		return DocDisplaySearchWindow{}, ErrIntegrity
	}
	return buildDocDisplaySearchWindow(previous, current)
}

func buildDocDisplaySearchWindow(previous *DocDisplayPage, current DocDisplayPage) (DocDisplaySearchWindow, error) {
	current.Runs = append([]DocDisplayRun(nil), current.Runs...)
	var text strings.Builder
	var parts []docFindDisplayPart
	from := current.From
	if previous != nil {
		if previous.Source != current.Source || previous.Version != current.Version || previous.Index+1 != current.Index || previous.From+len(previous.Text) != current.From {
			return DocDisplaySearchWindow{}, ErrIntegrity
		}
		copy := *previous
		copy.Runs = append([]DocDisplayRun(nil), previous.Runs...)
		start, characters := len(copy.Text), 0
		for start > 0 && characters < MaxDocFindQueryRunes-1 {
			_, width := utf8.DecodeLastRuneInString(copy.Text[:start])
			start -= width
			characters++
		}
		prefix := docDisplayPageSlice{page: copy, start: start}
		text.WriteString(prefix.Text())
		parts = append(parts, docFindDisplayPart{from: 0, to: text.Len(), display: prefix})
		from -= text.Len()
	}
	start := text.Len()
	text.WriteString(current.Text)
	parts = append(parts, docFindDisplayPart{from: start, to: text.Len(), display: docDisplayPageSlice{page: current}})
	return DocDisplaySearchWindow{from: from, boundary: current.From, display: docFindCompositeDisplay{text: text.String(), parts: parts}}, nil
}

func FindDocDisplayWindow(ctx context.Context, window DocDisplaySearchWindow, query string, start, limit int) (matches []DocFindMatch, next int, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			matches, next, failure = nil, 0, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if window.display == nil || start < 0 || start > window.To() || limit < 1 || limit > DocFindPageMatches || ValidateDocFindQuery(query) != nil {
		return nil, 0, ErrValidation
	}
	position := max(start-window.from, 0)
	for position < len(window.Text()) {
		found, err := FindDocText(ctx, window.Text(), query, position, DocFindPageMatches)
		if err != nil {
			return nil, 0, err
		}
		if len(found) == 0 {
			return matches, window.To(), nil
		}
		for _, match := range found {
			_, width := utf8.DecodeRuneInString(window.Text()[match.Offset:])
			position = match.Offset + width
			if window.from+match.Offset+match.Length <= window.boundary {
				continue
			}
			match, err = WithDocFindPreview(window.Text(), match)
			if err != nil {
				return nil, 0, err
			}
			span, err := window.display.SourceRange(match.Offset, match.Length)
			if err != nil {
				return nil, 0, err
			}
			match.SourceRange = &span
			match.Offset += window.from
			match.PreviewOffset += window.from
			matches = append(matches, match)
			if len(matches) == limit {
				if match.Offset+match.Length == window.To() {
					return matches, window.To(), nil
				}
				return matches, window.from + position, nil
			}
		}
	}
	return matches, window.To(), nil
}
