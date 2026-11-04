package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

const DocDisplayStringProfile = "tool-result-string-v1"
const DocDisplayPageTextBytes = 32 << 10
const DocDisplayPageRuns = 512
const MaxDocDisplayPageBytes = 128 << 10
const MaxDocDisplayStateBytes = 64 << 10
const MaxDocDisplaySourceBytes = 2048

type DocDisplayStringSource struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
	Token      DocFindSpan `json:"token"`
	Empty      bool        `json:"empty"`
}

func (source DocDisplayStringSource) Validate() error {
	if source.Version != 1 || source.Profile != DocDisplayStringProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.Token.From < 0 || source.Token.From > source.EventBytes-2 || source.Token.To <= source.Token.From || source.Token.To > source.EventBytes || source.Token.To-source.Token.From < 2 || source.Token.To-source.Token.From == 2 && !source.Empty {
		return ErrIntegrity
	}
	return nil
}

func (source DocDisplayStringSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (source DocDisplayStringSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocDisplayStringSource(expected ContentHash, raw []byte) (DocDisplayStringSource, error) {
	var source DocDisplayStringSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocDisplayStringSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayStringSource{}, ErrIntegrity
	}
	return source, nil
}

type DocDisplayRun struct {
	To         int  `json:"to"`
	SourceFrom int  `json:"source_from"`
	SourceTo   int  `json:"source_to"`
	Linear     bool `json:"linear"`
}

type DocDisplayPage struct {
	Version int             `json:"version"`
	Source  ContentHash     `json:"source"`
	Index   int             `json:"index"`
	From    int             `json:"from"`
	Text    string          `json:"text"`
	Runs    []DocDisplayRun `json:"runs"`
}

func (page DocDisplayPage) Validate(source DocDisplayStringSource) error {
	hash, err := source.Hash()
	if err != nil || source.Empty {
		return ErrIntegrity
	}
	return page.validateJSONString(1, hash, source.Token)
}

func (page DocDisplayPage) validateJSONString(version int, source ContentHash, token DocFindSpan) error {
	if page.Version != version || page.Source != source || page.Index < 0 || page.Index >= MaxDocRangePages || page.From < page.Index || page.Index > page.From/DocDisplayPageRuns || page.From > token.To-token.From-2 || len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || !utf8.ValidString(page.Text) || len(page.Runs) == 0 || len(page.Runs) > DocDisplayPageRuns || page.From > token.To-token.From-2-len(page.Text) {
		return ErrIntegrity
	}
	if page.Index == 0 && (page.From != 0 || page.Runs[0].SourceFrom != token.From+1) {
		return ErrIntegrity
	}
	previous, rawEnd := 0, 0
	for index, run := range page.Runs {
		if run.To <= previous || run.To > len(page.Text) || run.To < len(page.Text) && !utf8.RuneStart(page.Text[run.To]) || run.SourceFrom <= token.From || run.SourceTo >= token.To || run.SourceFrom >= run.SourceTo || index > 0 && run.SourceFrom != rawEnd {
			return ErrIntegrity
		}
		if index == 0 && !validDocJSONStringByteRatio(run.SourceFrom-token.From-1, page.From) {
			return ErrIntegrity
		}
		if !validDocDisplayRunText(run, page.Text[previous:run.To]) {
			return ErrIntegrity
		}
		previous, rawEnd = run.To, run.SourceTo
	}
	if previous != len(page.Text) {
		return ErrIntegrity
	}
	if rawEnd < token.To-1 && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
		return ErrIntegrity
	}
	return nil
}

func validDocJSONStringByteRatio(raw, decoded int) bool {
	return raw >= 0 && decoded >= 0 && raw >= decoded && int64(raw) <= int64(decoded)*6
}

func (page DocDisplayPage) Encode(source DocDisplayStringSource) ([]byte, error) {
	if page.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayPage(source DocDisplayStringSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || unmarshalDocDisplayPage(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func (page DocDisplayPage) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(page.Text) || offset > len(page.Text)-length || !utf8.RuneStart(page.Text[offset]) || offset+length < len(page.Text) && !utf8.RuneStart(page.Text[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	if page.Version != 1 && page.Version != 2 && page.Version != 3 && page.Version != 4 {
		return DocFindSpan{}, ErrIntegrity
	}
	return page.validateCoordinateRange(MaxConversationDocumentBytes, page.Version == 1 || page.Version == 3, offset, offset+length)
}

func (page DocDisplayPage) validateCoordinates(sourceBytes int, literal bool) error {
	_, err := page.validateCoordinateRange(sourceBytes, literal, 0, 0)
	return err
}

func (page DocDisplayPage) validateCoordinateRange(sourceBytes int, literal bool, offset, end int) (DocFindSpan, error) {
	if len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || !utf8.ValidString(page.Text) || len(page.Runs) == 0 || len(page.Runs) > DocDisplayPageRuns {
		return DocFindSpan{}, ErrIntegrity
	}
	span := DocFindSpan{From: sourceBytes}
	previous, sourceEnd := 0, 0
	for index, run := range page.Runs {
		if run.To <= previous || run.To > len(page.Text) || run.To < len(page.Text) && !utf8.RuneStart(page.Text[run.To]) || run.SourceFrom < 0 || run.SourceFrom >= run.SourceTo || run.SourceTo > sourceBytes || literal && index > 0 && run.SourceFrom != sourceEnd {
			return DocFindSpan{}, ErrIntegrity
		}
		if run.Linear && run.SourceTo-run.SourceFrom != run.To-previous || literal && !run.Linear && !validDocDisplayRunText(run, page.Text[previous:run.To]) {
			return DocFindSpan{}, ErrIntegrity
		}
		if previous < end && run.To > offset {
			from, to := run.SourceFrom, run.SourceTo
			if run.Linear {
				from += max(offset-previous, 0)
				to -= max(run.To-end, 0)
			}
			span.From, span.To = min(span.From, from), max(span.To, to)
		}
		previous, sourceEnd = run.To, run.SourceTo
	}
	if previous != len(page.Text) {
		return DocFindSpan{}, ErrIntegrity
	}
	if end == 0 {
		return DocFindSpan{}, nil
	}
	return span, nil
}

func validDocDisplayRunText(run DocDisplayRun, text string) bool {
	width := run.SourceTo - run.SourceFrom
	if run.Linear {
		if width != len(text) {
			return false
		}
		for _, character := range text {
			if character < 0x20 || character == '"' || character == '\\' {
				return false
			}
		}
		return true
	}
	character, runeBytes := utf8.DecodeRuneInString(text)
	if runeBytes == 0 || runeBytes != len(text) {
		return false
	}
	switch width {
	case 2:
		return character == '"' || character == '\\' || character == '/' || character == '\b' || character == '\f' || character == '\n' || character == '\r' || character == '\t'
	case 6:
		return character <= 0xffff
	case 12:
		return character > 0xffff
	default:
		return false
	}
}

type DocDisplayStringState struct {
	Version    int                          `json:"version"`
	Source     ContentHash                  `json:"source"`
	Next       int                          `json:"next"`
	TextBytes  int                          `json:"text_bytes"`
	Directory  DocRangeDirectoryAccumulator `json:"directory"`
	HasContent bool                         `json:"has_content"`
	Done       bool                         `json:"done"`
}

func NewDocDisplayStringState(source DocDisplayStringSource) (DocDisplayStringState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocDisplayStringState{}, err
	}
	state := DocDisplayStringState{Version: 1, Source: hash, Next: source.Token.From}
	if source.Empty {
		state.Next, state.Done = source.Token.To, true
	}
	return state, nil
}

func (state DocDisplayStringState) Validate(source DocDisplayStringSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Next < source.Token.From || state.Next > source.Token.To || state.Done != (state.Next == source.Token.To) || state.TextBytes < 0 || state.TextBytes > state.Next-source.Token.From || state.TextBytes > source.Token.To-source.Token.From-2 || state.Directory.Validate() != nil || state.Directory.Count > state.TextBytes || state.Directory.Count == 0 && state.TextBytes != 0 || state.TextBytes == 0 && state.HasContent {
		return ErrIntegrity
	}
	if state.Next == source.Token.From && (state.TextBytes != 0 || state.Directory.Count != 0) {
		return ErrIntegrity
	}
	if !source.Empty && state.Directory.Count == 0 && state.Next != source.Token.From {
		return ErrIntegrity
	}
	if source.Empty && (!state.Done || state.TextBytes != 0 || state.Directory.Count != 0 || state.HasContent) || !source.Empty && state.Done && !state.HasContent {
		return ErrIntegrity
	}
	if state.Directory.Count > 0 && state.Directory.Count-1 > (state.TextBytes-1)/DocDisplayPageRuns {
		return ErrIntegrity
	}
	if !source.Empty && state.Directory.Count > 0 {
		consumed := state.Next - source.Token.From - 1
		if state.Done {
			consumed--
		}
		if state.TextBytes > consumed || int64(consumed) > int64(state.TextBytes)*6 {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocDisplayStringState) Encode(source DocDisplayStringSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayStringState(source DocDisplayStringSource, expected ContentHash, raw []byte) (DocDisplayStringState, error) {
	var state DocDisplayStringState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocDisplayStringState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayStringState{}, ErrIntegrity
	}
	return state, nil
}

type DocDisplayStringGeneration struct {
	Version   int                    `json:"version"`
	Source    DocDisplayStringSource `json:"source"`
	TextBytes int                    `json:"text_bytes"`
	Pages     DocRangeDirectoryRoot  `json:"pages"`
}

func (generation DocDisplayStringGeneration) Validate() error {
	if generation.Version != 1 || generation.Source.Validate() != nil || generation.TextBytes < 0 || generation.TextBytes > generation.Source.Token.To-generation.Source.Token.From-2 || generation.Pages.validate(generation.Pages.Count) != nil || generation.Pages.Count > generation.TextBytes || (generation.TextBytes == 0) != (generation.Pages.Count == 0) || (generation.TextBytes == 0) != generation.Source.Empty {
		return ErrIntegrity
	}
	if generation.Pages.Count > 0 && generation.Pages.Count-1 > (generation.TextBytes-1)/DocDisplayPageRuns {
		return ErrIntegrity
	}
	return nil
}

func (generation DocDisplayStringGeneration) Encode() ([]byte, error) {
	if generation.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(generation)
}

func DecodeDocDisplayStringGeneration(expected ContentHash, raw []byte) (DocDisplayStringGeneration, error) {
	var generation DocDisplayStringGeneration
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &generation) != nil {
		return DocDisplayStringGeneration{}, ErrIntegrity
	}
	canonical, err := generation.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayStringGeneration{}, ErrIntegrity
	}
	return generation, nil
}

func FinishDocDisplayString(ctx context.Context, source DocDisplayStringSource, state DocDisplayStringState) (DocDisplayStringGeneration, []DocRangeObject, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayStringGeneration{}, nil, err
	}
	if state.Validate(source) != nil || !state.Done {
		return DocDisplayStringGeneration{}, nil, ErrIntegrity
	}
	generation := DocDisplayStringGeneration{Version: 1, Source: source}
	if !state.HasContent {
		return generation, nil, nil
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocDisplayStringGeneration{}, nil, err
	}
	generation.Pages, generation.TextBytes = root, state.TextBytes
	if generation.Validate() != nil {
		return DocDisplayStringGeneration{}, nil, ErrIntegrity
	}
	return generation, objects, nil
}

func ReadDocDisplayPage(ctx context.Context, generation DocDisplayStringGeneration, index int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if generation.Validate() != nil || index < 0 || index >= generation.Pages.Count || load == nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, generation.Pages, index, load)
	if err != nil {
		return DocDisplayPage{}, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return DocDisplayPage{}, err
	}
	page, err := DecodeDocDisplayPage(generation.Source, hash, raw)
	if err != nil || page.Index != index || index == 0 && page.From != 0 || page.From > generation.TextBytes-len(page.Text) || index == generation.Pages.Count-1 && page.From+len(page.Text) != generation.TextBytes {
		return DocDisplayPage{}, ErrIntegrity
	}
	if index == generation.Pages.Count-1 && page.Runs[len(page.Runs)-1].SourceTo != generation.Source.Token.To-1 {
		return DocDisplayPage{}, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	return page, nil
}
