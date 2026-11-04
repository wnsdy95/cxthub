package domain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DocFindProjection = "find-v1"
const DocLegacyFindProjection = "find-legacy-v1"
const DocFindDisplayRepresentation = "display-v1"
const DocFindDisplayProjection = "find-display-v1"
const DocLegacyFindDisplayProjection = "find-legacy-display-v1"
const DocFindPreparedRepresentation = "display-prepared-v1"
const DocFindPreparedProjection = "find-display-prepared-v1"
const DocLegacyFindPreparedProjection = "find-legacy-display-prepared-v1"
const MaxDocFindSelectorBytes = 4096
const MaxDocFindQueryRunes = 256
const DocFindPageMatches = 20

var ErrDocFindFieldsUnavailable = errors.New("bounded searchable fields are unavailable for this event")

var ErrDocFindFieldsPending = errors.New("bounded searchable fields are being prepared")
var ErrDocDisplayPending = errors.New("displayed event pages are being prepared")

type DocFindPreparedProgress struct {
	Page       int         `json:"page"`
	Generation ContentHash `json:"generation"`
}

type DocFindPageRequest struct {
	Representation string
	Selection      string
	Cursor         string
	Query          string
	From           int
	To             int
	Legacy         bool
	Base           ContentHash
	Total          int
	Inherited      int
}

type DocFindSelector struct {
	Prepared       *DocFindPreparedProgress `json:"prepared,omitempty"`
	Representation string                   `json:"representation,omitempty"`
	Version        int                      `json:"version"`
	Selection      DocVisibleSelector       `json:"selection"`
	QueryHash      ContentHash              `json:"query_hash"`
	From           int                      `json:"from"`
	To             int                      `json:"to"`
	Index          int                      `json:"index"`
	Field          int                      `json:"field"`
	Byte           int                      `json:"byte"`
}

type DocFindMatch struct {
	PreparedPage  *int         `json:"prepared_page,omitempty"`
	Index         int          `json:"event_index"`
	Field         int          `json:"field"`
	Offset        int          `json:"byte_offset"`
	Length        int          `json:"byte_length"`
	Raw           bool         `json:"raw"`
	Preview       string       `json:"preview"`
	PreviewOffset int          `json:"preview_byte_offset"`
	RevealCursor  string       `json:"reveal_cursor"`
	SourceRange   *DocFindSpan `json:"source_range,omitempty"`
}

type DocFindPage struct {
	Prepared   *DocFindPreparedProgress `json:"prepared,omitempty"`
	Projection string                   `json:"projection"`
	Hash       ContentHash              `json:"hash"`
	Generation ContentHash              `json:"generation"`
	From       int                      `json:"from"`
	To         int                      `json:"to"`
	Index      int                      `json:"event_index"`
	Field      int                      `json:"field"`
	Byte       int                      `json:"byte_offset"`
	Matches    []DocFindMatch           `json:"matches"`
	NextCursor *string                  `json:"next_cursor"`
}

func ValidateDocFindQuery(query string) error {
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) < 1 || utf8.RuneCountInString(query) > MaxDocFindQueryRunes {
		return ErrValidation
	}
	return nil
}

func (selector DocFindSelector) Validate() error {
	if !validDocFindRepresentation(selector.Version, selector.Representation) || selector.Selection.Validate() != nil || selector.Selection.Purpose != "units" ||
		ValidateContentHash(selector.QueryHash) != nil || selector.From < selector.Selection.Start || selector.To < selector.From ||
		selector.To > selector.Selection.Total || selector.Index < selector.From || selector.Index > selector.To ||
		selector.Field < 0 || selector.Field > 128<<10 || selector.Byte < 0 || selector.Byte > MaxConversationDocumentBytes ||
		selector.Representation == DocFindDisplayRepresentation && (selector.Field != 0 || selector.Byte > MaxDocFindDisplayBytes) ||
		!validDocFindPreparedProgress(selector.Representation, selector.Prepared, selector.Field, selector.Byte, selector.Index == selector.To) ||
		selector.Index == selector.To && (selector.Field != 0 || selector.Byte != 0) {
		return ErrValidation
	}
	return nil
}

func validDocFindRepresentation(version int, representation string) bool {
	return version == 1 && representation == "" || version == 2 && representation == DocFindDisplayRepresentation || version == 3 && representation == DocFindPreparedRepresentation
}

func validDocFindPreparedProgress(representation string, progress *DocFindPreparedProgress, field, position int, done bool) bool {
	if representation != DocFindPreparedRepresentation {
		return progress == nil
	}
	return progress != nil && field == 0 && progress.Page >= 0 && progress.Page < MaxDocRangePages &&
		ValidateOptionalContentHash(progress.Generation) == nil && (progress.Page == 0 || progress.Generation != "") &&
		(progress.Generation != "" || position <= MaxDocFindDisplayBytes) && (!done || progress.Page == 0 && progress.Generation == "")
}

func (selector DocFindSelector) Encode() (string, error) {
	if err := selector.Validate(); err != nil {
		return "", err
	}
	raw, err := canonicalJSON(selector)
	if err != nil || len(raw) > MaxDocFindSelectorBytes {
		return "", ErrValidation
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeDocFindSelector(encoded string) (DocFindSelector, error) {
	var selector DocFindSelector
	if len(encoded) == 0 || len(encoded) > base64.RawURLEncoding.EncodedLen(MaxDocFindSelectorBytes) {
		return selector, ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > MaxDocFindSelectorBytes || json.Unmarshal(raw, &selector) != nil || selector.Validate() != nil {
		return DocFindSelector{}, ErrValidation
	}
	canonical, err := canonicalJSON(selector)
	if err != nil || !bytes.Equal(canonical, raw) || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return DocFindSelector{}, ErrValidation
	}
	return selector, nil
}

func DocFindTexts(event CIREvent) []string {
	var fields []string
	appendText := func(text string) {
		if text != "" {
			fields = append(fields, text)
		}
	}
	switch event.Kind {
	case EventMessage, EventTurn:
		for _, block := range event.Blocks {
			if block.Type == "text" {
				appendText(block.Text)
			}
		}
	case EventReasoning:
		appendText(event.RedactedSummary)
	case EventToolCall:
		if event.ProviderToolName != "" {
			appendText(event.ProviderToolName)
		} else {
			appendText(event.ToolName)
		}
		if event.Input != nil {
			appendDocFindValues(event.Input, &fields)
		}
	case EventToolResult:
		if event.Output != nil {
			appendDocFindValues(event.Output, &fields)
		}
	}
	return fields
}

func appendDocFindValues(value any, fields *[]string) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*fields = append(*fields, typed)
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key != "" {
				*fields = append(*fields, key)
			}
			appendDocFindValues(typed[key], fields)
		}
	case []any:
		for _, item := range typed {
			appendDocFindValues(item, fields)
		}
	case nil:
		*fields = append(*fields, "null")
	default:
		raw, err := json.Marshal(typed)
		if err == nil {
			*fields = append(*fields, string(raw))
		}
	}
}

func FindDocText(ctx context.Context, text, query string, start, limit int) ([]DocFindMatch, error) {
	if ValidateDocFindQuery(query) != nil || start < 0 || start > len(text) || start < len(text) && !utf8.RuneStart(text[start]) ||
		!utf8.ValidString(text) || limit < 1 || limit > DocFindPageMatches {
		return nil, ErrValidation
	}
	pattern := []rune(query)
	for index := range pattern {
		pattern[index] = foldFindRune(pattern[index])
	}
	failure := make([]int, len(pattern))
	for index, matched := 1, 0; index < len(pattern); index++ {
		for matched > 0 && pattern[index] != pattern[matched] {
			matched = failure[matched-1]
		}
		if pattern[index] == pattern[matched] {
			matched++
		}
		failure[index] = matched
	}
	positions := make([]int, len(pattern))
	var matches []DocFindMatch
	matched, ordinal := 0, 0
	for offset := start; offset < len(text); {
		if ordinal%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		current, size := utf8.DecodeRuneInString(text[offset:])
		current = foldFindRune(current)
		positions[ordinal%len(pattern)] = offset
		for matched > 0 && current != pattern[matched] {
			matched = failure[matched-1]
		}
		if current == pattern[matched] {
			matched++
		}
		if matched == len(pattern) {
			begin := positions[(ordinal-len(pattern)+1)%len(pattern)]
			matches = append(matches, DocFindMatch{Offset: begin, Length: offset + size - begin})
			if len(matches) == limit {
				return matches, nil
			}
			matched = failure[matched-1]
		}
		offset += size
		ordinal++
	}
	return matches, ctx.Err()
}

func foldFindRune(value rune) rune {
	minimum := value
	for next := unicode.SimpleFold(value); next != value; next = unicode.SimpleFold(next) {
		if next < minimum {
			minimum = next
		}
	}
	return minimum
}

func WithDocFindPreview(text string, match DocFindMatch) (DocFindMatch, error) {
	if match.Offset < 0 || match.Length <= 0 || match.Offset > len(text)-match.Length || !utf8.ValidString(text) ||
		!utf8.RuneStart(text[match.Offset]) || match.Offset+match.Length < len(text) && !utf8.RuneStart(text[match.Offset+match.Length]) ||
		utf8.RuneCountInString(text[match.Offset:match.Offset+match.Length]) > MaxDocFindQueryRunes {
		return DocFindMatch{}, ErrValidation
	}
	begin, end := match.Offset, match.Offset+match.Length
	for count := 0; count < 32 && begin > 0; count++ {
		_, size := utf8.DecodeLastRuneInString(text[:begin])
		begin -= size
	}
	for count := 0; count < 32 && end < len(text); count++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	match.Preview, match.PreviewOffset = strings.Clone(text[begin:end]), begin
	return match, nil
}
