package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

const DocToolStringProfile = "public-tool-string-v1"
const docToolStringPageMinRunes = 256

type DocToolStringMode string

const (
	DocToolStringQuoted DocToolStringMode = "quoted"
	DocToolStringAdd    DocToolStringMode = "add"
	DocToolStringRemove DocToolStringMode = "remove"
)

type DocToolStringSource struct {
	Version    int               `json:"version"`
	Profile    string            `json:"profile"`
	EventHash  ContentHash       `json:"event_hash"`
	EventBytes int               `json:"event_bytes"`
	Token      DocFindSpan       `json:"token"`
	Mode       DocToolStringMode `json:"mode"`
}

func (source DocToolStringSource) Validate() error {
	if source.Version != 1 || source.Profile != DocToolStringProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.Token.From < 0 || source.Token.From > source.EventBytes-2 || source.Token.To <= source.Token.From || source.Token.To > source.EventBytes || source.Token.To-source.Token.From < 2 {
		return ErrIntegrity
	}
	switch source.Mode {
	case DocToolStringQuoted, DocToolStringAdd, DocToolStringRemove:
		return nil
	default:
		return ErrIntegrity
	}
}

func (source DocToolStringSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolStringSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolStringSource(expected ContentHash, raw []byte) (DocToolStringSource, error) {
	var source DocToolStringSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolStringSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolStringSource{}, ErrIntegrity
	}
	return source, nil
}

type DocToolStringPage struct {
	Version int            `json:"version"`
	Source  ContentHash    `json:"source"`
	RawFrom int            `json:"raw_from"`
	RawTo   int            `json:"raw_to"`
	Display DocDisplayPage `json:"display"`
}

func (page DocToolStringPage) SourceRange(offset, length int) (DocFindSpan, error) {
	if page.Version != 1 || page.Display.Version != 5 || page.Source != page.Display.Source {
		return DocFindSpan{}, ErrIntegrity
	}
	display := page.Display
	display.Version = 2
	return display.SourceRange(offset, length)
}

func (page DocToolStringPage) Validate(source DocToolStringSource) error {
	hash, err := source.Hash()
	display := page.Display
	if err != nil || page.Version != 1 || page.Source != hash || display.Version != 5 || display.Source != hash || page.RawFrom < source.Token.From || page.RawTo > source.Token.To || page.RawTo <= page.RawFrom || display.Index < 0 || display.Index >= MaxDocRangePages || display.From < 0 || display.From > MaxConversationDocumentBytes-len(display.Text) || display.Index > display.From/docToolStringPageMinRunes || (display.Index == 0) != (display.From == 0) || (display.Index == 0) != (page.RawFrom == source.Token.From) || display.validateCoordinates(source.EventBytes, false) != nil {
		return ErrIntegrity
	}
	if page.RawTo < source.Token.To && utf8.RuneCountInString(display.Text) < docToolStringPageMinRunes || int64(len(display.Text)) > int64(page.RawTo-page.RawFrom)*6+4 {
		return ErrIntegrity
	}
	for _, run := range display.Runs {
		if run.SourceFrom < page.RawFrom || run.SourceTo > page.RawTo {
			return ErrIntegrity
		}
	}
	return nil
}

func (page DocToolStringPage) Encode(source DocToolStringSource) ([]byte, error) {
	if page.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolStringPage(source DocToolStringSource, expected ContentHash, raw []byte) (DocToolStringPage, error) {
	var page DocToolStringPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocToolStringPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolStringPage{}, ErrIntegrity
	}
	return page, nil
}

type DocToolStringState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	Next      int                          `json:"next"`
	TextBytes int                          `json:"text_bytes"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
	Done      bool                         `json:"done"`
}

func NewDocToolStringState(source DocToolStringSource) (DocToolStringState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocToolStringState{}, err
	}
	return DocToolStringState{Version: 1, Source: hash, Next: source.Token.From}, nil
}

func (state DocToolStringState) Validate(source DocToolStringSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Next < source.Token.From || state.Next > source.Token.To || state.Done != (state.Next == source.Token.To) || state.Directory.Validate() != nil || !validDocToolStringPageCount(state.Directory.Count, state.TextBytes, state.Done) || (state.Next == source.Token.From) != (state.Directory.Count == 0) || state.Next == source.Token.To-1 || int64(state.TextBytes) > int64(state.Next-source.Token.From)*6+4 {
		return ErrIntegrity
	}
	return nil
}

func (state DocToolStringState) Encode(source DocToolStringSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolStringState(source DocToolStringSource, expected ContentHash, raw []byte) (DocToolStringState, error) {
	var state DocToolStringState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolStringState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolStringState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolStringPlan struct {
	Version   int                   `json:"version"`
	Source    DocToolStringSource   `json:"source"`
	Pages     DocRangeDirectoryRoot `json:"pages"`
	TextBytes int                   `json:"text_bytes"`
}

func validDocToolStringPageCount(count, textBytes int, done bool) bool {
	if count < 0 || count > MaxDocRangePages || textBytes < 0 || textBytes > MaxConversationDocumentBytes || (count == 0) != (textBytes == 0) {
		return false
	}
	if count == 0 {
		return true
	}
	return int64(textBytes) <= int64(count)*DocDisplayPageTextBytes && count-1 <= (textBytes-1)/docToolStringPageMinRunes && (done || count <= textBytes/docToolStringPageMinRunes)
}

func (plan DocToolStringPlan) Validate() error {
	if plan.Version != 1 || plan.Source.Validate() != nil || plan.Pages.validate(plan.Pages.Count) != nil || plan.Pages.Count == 0 || !validDocToolStringPageCount(plan.Pages.Count, plan.TextBytes, true) || int64(plan.TextBytes) > int64(plan.Source.Token.To-plan.Source.Token.From)*6+4 {
		return ErrIntegrity
	}
	return nil
}

func (plan DocToolStringPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolStringPlan(expected ContentHash, raw []byte) (DocToolStringPlan, error) {
	var plan DocToolStringPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocToolStringPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolStringPlan{}, ErrIntegrity
	}
	return plan, nil
}

func ReadDocToolStringPage(ctx context.Context, plan DocToolStringPlan, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocToolStringPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolStringPage{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if plan.Validate() != nil || index < 0 || index >= plan.Pages.Count || load == nil {
		return result, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, plan.Pages, index, load)
	if err != nil {
		return result, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return result, err
	}
	page, err := DecodeDocToolStringPage(plan.Source, hash, raw)
	if err != nil || page.Display.Index != index || len(page.Display.Text) > plan.TextBytes-page.Display.From || (page.Display.From+len(page.Display.Text) == plan.TextBytes) != (index == plan.Pages.Count-1) || (page.RawTo == plan.Source.Token.To) != (index == plan.Pages.Count-1) {
		return result, ErrIntegrity
	}
	return page, nil
}
