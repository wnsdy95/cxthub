package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

const DocToolValueProfile = "public-tool-value-plan-v1"
const DocToolValuePageMinRunes = 256

type DocToolValueSource struct {
	Version int                 `json:"version"`
	Profile string              `json:"profile"`
	Tree    DocToolTreeSource   `json:"tree"`
	Keys    DocToolKeyMapSource `json:"keys"`
	Orders  DocToolOrdersSource `json:"orders"`
	Root    DocToolTreeNode     `json:"root"`
}

func NewDocToolValueSource(tree DocToolTreeSource, keys DocToolKeyMapSource, orders DocToolOrdersSource, root DocToolTreeNode) (DocToolValueSource, error) {
	source := DocToolValueSource{Version: 1, Profile: DocToolValueProfile, Tree: tree, Keys: keys, Orders: orders, Root: cloneDocToolTreeNode(root)}
	return source, source.Validate()
}

func (source DocToolValueSource) Validate() error {
	if source.Version != 1 || source.Profile != DocToolValueProfile || source.Tree.Validate() != nil || source.Keys.Validate(source.Tree) != nil || source.Orders.Validate(source.Tree, source.Keys) != nil || source.Root.Validate(source.Tree) != nil || source.Root.Depth != 0 {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolValueSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (source DocToolValueSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolValueSource(expected ContentHash, raw []byte) (DocToolValueSource, error) {
	var source DocToolValueSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolValueSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolValueSource{}, ErrIntegrity
	}
	return source, nil
}

type DocToolValuePage struct {
	Version int            `json:"version"`
	Source  ContentHash    `json:"source"`
	Sealed  bool           `json:"sealed"`
	Final   bool           `json:"final"`
	Display DocDisplayPage `json:"display"`
}

func (page DocToolValuePage) validatePosition() error {
	display := page.Display
	if page.Version != 1 || ValidateContentHash(page.Source) != nil || display.Version != 6 || display.Source != page.Source || page.Final && !page.Sealed || display.Index < 0 || display.Index >= MaxDocRangePages || display.From < 0 || display.From > MaxConversationDocumentBytes-len(display.Text) || display.Index > display.From/DocToolValuePageMinRunes || int64(display.From) > int64(display.Index)*DocDisplayPageTextBytes || (display.Index == 0) != (display.From == 0) || page.Sealed && !page.Final && utf8.RuneCountInString(display.Text) < DocToolValuePageMinRunes {
		return ErrIntegrity
	}
	return nil
}

func (page DocToolValuePage) SourceRange(offset, length int) (DocFindSpan, error) {
	if page.validatePosition() != nil {
		return DocFindSpan{}, ErrIntegrity
	}
	display := page.Display
	display.Version = 2
	return display.SourceRange(offset, length)
}

func (page DocToolValuePage) Validate(source DocToolValueSource) error {
	hash, err := source.Hash()
	if err != nil || page.Source != hash || page.validatePosition() != nil || page.Display.validateCoordinates(source.Tree.EventBytes, false) != nil {
		return ErrIntegrity
	}
	for _, run := range page.Display.Runs {
		if run.SourceFrom < source.Root.Value.From || run.SourceTo > source.Root.Value.To {
			return ErrIntegrity
		}
	}
	return nil
}

func (page DocToolValuePage) Encode(source DocToolValueSource) ([]byte, error) {
	if page.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolValuePage(source DocToolValueSource, expected ContentHash, raw []byte) (DocToolValuePage, error) {
	var page DocToolValuePage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocToolValuePage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolValuePage{}, ErrIntegrity
	}
	return page, nil
}

type DocToolValuePlan struct {
	Version   int                   `json:"version"`
	Source    DocToolValueSource    `json:"source"`
	Pages     DocRangeDirectoryRoot `json:"pages"`
	TextBytes int                   `json:"text_bytes"`
}

func validDocToolValuePageCount(count, textBytes int) bool {
	if count < 0 || count > MaxDocRangePages || textBytes < 0 || textBytes > MaxConversationDocumentBytes || (count == 0) != (textBytes == 0) {
		return false
	}
	return count == 0 || int64(textBytes) <= int64(count)*DocDisplayPageTextBytes && count-1 <= (textBytes-1)/DocToolValuePageMinRunes
}

func (plan DocToolValuePlan) Validate() error {
	if plan.Version != 1 || plan.Source.Validate() != nil || plan.Pages.validate(plan.Pages.Count) != nil || plan.Pages.Count == 0 || !validDocToolValuePageCount(plan.Pages.Count, plan.TextBytes) {
		return ErrIntegrity
	}
	return nil
}

func (plan DocToolValuePlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (plan DocToolValuePlan) Hash() (ContentHash, error) {
	raw, err := plan.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolValuePlan(expected ContentHash, raw []byte) (DocToolValuePlan, error) {
	var plan DocToolValuePlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocToolValuePlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolValuePlan{}, ErrIntegrity
	}
	return plan, nil
}

func ReadDocToolValuePage(ctx context.Context, plan DocToolValuePlan, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocToolValuePage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolValuePage{}
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
	page, err := DecodeDocToolValuePage(plan.Source, hash, raw)
	if err != nil || !page.Sealed || page.Display.Index != index || page.Final != (index == plan.Pages.Count-1) || !validDocToolValuePageCount(plan.Pages.Count-index-1, plan.TextBytes-page.Display.From-len(page.Display.Text)) {
		return result, ErrIntegrity
	}
	return page, nil
}
