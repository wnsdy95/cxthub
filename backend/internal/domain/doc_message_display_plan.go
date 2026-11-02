package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocMessageDisplayProfile = "public-message-display-plan-v1"
const DocMessageDisplayPageParts = 64
const MaxDocMessageDisplayPageBytes = 32 << 10
const DocMessageDisplayStepBlocks = 64

type DocMessageDisplaySource struct {
	Version int                       `json:"version"`
	Profile string                    `json:"profile"`
	Input   DocMessageInputGeneration `json:"input"`
}

func (source DocMessageDisplaySource) Validate() error {
	if source.Version != 1 || source.Profile != DocMessageDisplayProfile || source.Input.Validate() != nil || source.Input.Source.Summary {
		return ErrIntegrity
	}
	return nil
}

func (source DocMessageDisplaySource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (source DocMessageDisplaySource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocMessageDisplaySource(expected ContentHash, raw []byte) (DocMessageDisplaySource, error) {
	var source DocMessageDisplaySource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocMessageDisplaySource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageDisplaySource{}, ErrIntegrity
	}
	return source, nil
}

type DocMessageDisplayKind string

const (
	DocMessageDisplayMarkdown    DocMessageDisplayKind = "markdown"
	DocMessageDisplayPlaceholder DocMessageDisplayKind = "placeholder"
)

type DocMessageDisplayPart struct {
	Index     int                   `json:"index"`
	Block     int                   `json:"block"`
	From      int                   `json:"from"`
	Separator int                   `json:"separator"`
	TextBytes int                   `json:"text_bytes"`
	Kind      DocMessageDisplayKind `json:"kind"`
	Plan      ContentHash           `json:"plan"`
	First     DocFindSpan           `json:"first"`
}

func (part DocMessageDisplayPart) end() int { return part.From + part.Separator + part.TextBytes }

func (part DocMessageDisplayPart) Validate(source DocMessageDisplaySource) error {
	if source.Validate() != nil || part.Index < 0 || part.Index >= source.Input.Count || part.Block < part.Index || part.Block >= source.Input.Count || part.From < 0 || part.From > MaxConversationDocumentBytes || int64(part.From) < int64(part.Index)*2-1 || part.Separator < 0 || part.Separator > 2 || part.Index == 0 && (part.From != 0 || part.Separator != 0) || part.Index > 0 && part.Separator == 0 || part.TextBytes <= 0 || part.TextBytes > MaxConversationDocumentBytes-part.From-part.Separator || part.Kind != DocMessageDisplayMarkdown && part.Kind != DocMessageDisplayPlaceholder || ValidateContentHash(part.Plan) != nil || part.First.From <= source.Input.Source.Blocks.From || part.First.To <= part.First.From || part.First.To >= source.Input.Source.Blocks.To || int64(part.First.From-source.Input.Source.Blocks.From-1) < int64(part.Block)*3 {
		return ErrIntegrity
	}
	return nil
}

type DocMessageDisplayPage struct {
	Version int                     `json:"version"`
	Source  ContentHash             `json:"source"`
	Index   int                     `json:"index"`
	Parts   []DocMessageDisplayPart `json:"parts"`
}

func (page DocMessageDisplayPage) Validate(source DocMessageDisplaySource) error {
	hash, err := source.Hash()
	if err != nil || page.Version != 1 || page.Source != hash || page.Index < 0 || page.Index >= MaxDocRangePages || len(page.Parts) == 0 || len(page.Parts) > DocMessageDisplayPageParts {
		return ErrIntegrity
	}
	for index, part := range page.Parts {
		if part.Validate(source) != nil || part.Index != page.Index*DocMessageDisplayPageParts+index || index > 0 && (part.From != page.Parts[index-1].end() || part.Block <= page.Parts[index-1].Block || part.First.From < page.Parts[index-1].First.To) {
			return ErrIntegrity
		}
	}
	return nil
}

func (page DocMessageDisplayPage) Encode(source DocMessageDisplaySource) ([]byte, error) {
	if page.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocMessageDisplayPage(page)
	if err != nil || len(raw) > MaxDocMessageDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessageDisplayPage(source DocMessageDisplaySource, expected ContentHash, raw []byte) (DocMessageDisplayPage, error) {
	var page DocMessageDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocMessageDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocMessageDisplayPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocMessageDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

type DocMessageDisplayPlan struct {
	Version   int                     `json:"version"`
	Source    DocMessageDisplaySource `json:"source"`
	Count     int                     `json:"count"`
	TextBytes int                     `json:"text_bytes"`
	Parts     DocRangeDirectoryRoot   `json:"parts"`
}

func (plan DocMessageDisplayPlan) Validate() error {
	if plan.Version != 1 || plan.Source.Validate() != nil || plan.Count < 0 || plan.Count > plan.Source.Input.Count || plan.TextBytes < 0 || plan.TextBytes > MaxConversationDocumentBytes || (plan.Count == 0) != (plan.TextBytes == 0) || int64(plan.TextBytes) < int64(plan.Count)*2-1 || plan.Parts.validate((plan.Count+DocMessageDisplayPageParts-1)/DocMessageDisplayPageParts) != nil {
		return ErrIntegrity
	}
	return nil
}

func (plan DocMessageDisplayPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessageDisplayPlan(expected ContentHash, raw []byte) (DocMessageDisplayPlan, error) {
	var plan DocMessageDisplayPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocMessageDisplayPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocMessageDisplayPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan DocMessageDisplayPlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.Input.Source.EventHash, EventBytes: plan.Source.Input.Source.EventBytes, Plan: HashContent(raw), TextBytes: plan.TextBytes}
	return source, source.Validate()
}

type docMessageDisplayPartReader struct {
	plan       DocMessageDisplayPlan
	cachedPlan DocMessageDisplayPlan
	index      int
	hash       ContentHash
	parts      []DocMessageDisplayPart
}

func ReadDocMessageDisplayParts(ctx context.Context, plan DocMessageDisplayPlan, index int, load func(context.Context, ContentHash) ([]byte, error)) ([]DocMessageDisplayPart, error) {
	reader := docMessageDisplayPartReader{plan: plan}
	return reader.read(ctx, index, load)
}

func (reader *docMessageDisplayPartReader) read(ctx context.Context, index int, load func(context.Context, ContentHash) ([]byte, error)) (result []DocMessageDisplayPart, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil || reader.plan.Validate() != nil || index < 0 || index >= reader.plan.Parts.Count || load == nil {
		return nil, ErrIntegrity
	}
	plan := reader.plan
	hash, err := ResolveDocRangeLeaf(ctx, plan.Parts, index, load)
	if err != nil {
		return nil, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return nil, err
	}
	if reader.cachedPlan == plan && reader.index == index && reader.hash == hash && len(reader.parts) > 0 {
		if len(raw) == 0 || len(raw) > MaxDocMessageDisplayPageBytes || ValidateContentHash(hash) != nil || HashContent(raw) != hash {
			return nil, ErrIntegrity
		}
		return reader.parts, nil
	}
	page, err := DecodeDocMessageDisplayPage(plan.Source, hash, raw)
	if err != nil || page.Index != index || len(page.Parts) != min(DocMessageDisplayPageParts, plan.Count-index*DocMessageDisplayPageParts) {
		return nil, ErrIntegrity
	}
	last := page.Parts[len(page.Parts)-1]
	remaining := plan.Count - last.Index - 1
	if last.end() > plan.TextBytes || remaining == 0 && last.end() != plan.TextBytes || int64(plan.TextBytes-last.end()) < int64(remaining)*2 {
		return nil, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result = append([]DocMessageDisplayPart(nil), page.Parts...)
	reader.cachedPlan, reader.index, reader.hash, reader.parts = plan, index, hash, result
	return result, nil
}

type DocMessageDisplayBlockPlan struct {
	Markdown    *DocDecodedMarkdownPlan
	Placeholder *DocMessagePlaceholderPlan
}
