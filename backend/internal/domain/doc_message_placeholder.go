package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

type DocMessagePlaceholderPlan struct {
	Version   int                       `json:"version"`
	Source    DocMessageInputSource     `json:"source"`
	Block     DocMessageBlockInput      `json:"block"`
	Type      *DocDecodedTextGeneration `json:"type,omitempty"`
	Name      *DocDecodedTextGeneration `json:"name,omitempty"`
	TextBytes int                       `json:"text_bytes"`
}

func NewDocMessagePlaceholderPlan(input DocMessageInput, blockType, name *DocDecodedTextGeneration) (DocMessagePlaceholderPlan, error) {
	plan := DocMessagePlaceholderPlan{Version: 1, Source: input.source, Block: input.entry, TextBytes: 2}
	if blockType != nil {
		copied := *blockType
		plan.Type = &copied
		plan.TextBytes += copied.TextBytes
	}
	if name != nil {
		copied := *name
		plan.Name = &copied
		if copied.TextBytes > 0 {
			plan.TextBytes += 2 + copied.TextBytes
		}
	}
	return plan, plan.Validate()
}

func (plan DocMessagePlaceholderPlan) Validate() error {
	if plan.Version != 1 || plan.Source.Validate() != nil || plan.Block.Validate(plan.Source) != nil || plan.Source.Summary || plan.Block.IsText || plan.TextBytes < 2 || plan.TextBytes > MaxConversationDocumentBytes {
		return ErrIntegrity
	}
	input := DocMessageInput{source: plan.Source, entry: plan.Block}
	blockType, name, err := input.PlaceholderSources()
	if err != nil {
		return err
	}
	length := 2
	for index, selected := range []*DocDecodedTextSource{blockType, name} {
		generation := []*DocDecodedTextGeneration{plan.Type, plan.Name}[index]
		if (selected == nil) != (generation == nil) {
			return ErrIntegrity
		}
		if generation != nil {
			if generation.Validate() != nil || generation.Source != *selected || generation.TextBytes > MaxConversationDocumentBytes-length {
				return ErrIntegrity
			}
			length += generation.TextBytes
			if index == 1 && generation.TextBytes > 0 {
				if length > MaxConversationDocumentBytes-2 {
					return ErrIntegrity
				}
				length += 2
			}
		}
	}
	if length != plan.TextBytes {
		return ErrIntegrity
	}
	return nil
}

func (plan DocMessagePlaceholderPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessagePlaceholderPlan(expected ContentHash, raw []byte) (DocMessagePlaceholderPlan, error) {
	var plan DocMessagePlaceholderPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocMessagePlaceholderPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessagePlaceholderPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan DocMessagePlaceholderPlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.EventHash, EventBytes: plan.Source.EventBytes, Plan: HashContent(raw), TextBytes: plan.TextBytes}
	return source, source.Validate()
}

type DocMessagePlaceholderCursor struct {
	Version int         `json:"version"`
	Plan    ContentHash `json:"plan"`
	From    int         `json:"from"`
}

func NewDocMessagePlaceholderCursor(plan DocMessagePlaceholderPlan) (DocMessagePlaceholderCursor, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocMessagePlaceholderCursor{}, err
	}
	return DocMessagePlaceholderCursor{Version: 1, Plan: source.Plan}, nil
}

type DocMessagePlaceholderFragment struct {
	text   string
	from   int
	anchor DocFindSpan
	next   DocMessagePlaceholderCursor
}

func (fragment DocMessagePlaceholderFragment) Text() string { return fragment.text }
func (fragment DocMessagePlaceholderFragment) From() int    { return fragment.from }
func (fragment DocMessagePlaceholderFragment) Next() DocMessagePlaceholderCursor {
	return fragment.next
}

func (fragment DocMessagePlaceholderFragment) SourceRange(offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(fragment.text) || offset > len(fragment.text)-length || !utf8.RuneStart(fragment.text[offset]) || offset+length < len(fragment.text) && !utf8.RuneStart(fragment.text[offset+length]) || fragment.anchor.From < 0 || fragment.anchor.To <= fragment.anchor.From {
		return DocFindSpan{}, ErrValidation
	}
	return fragment.anchor, nil
}

func ReadDocMessagePlaceholderFragment(ctx context.Context, plan DocMessagePlaceholderPlan, cursor DocMessagePlaceholderCursor, load func(context.Context, ContentHash) ([]byte, error)) (result DocMessagePlaceholderFragment, failure error) {
	return readDocMessagePlaceholderFragment(ctx, plan, cursor, load, DocDisplayPageTextBytes)
}

func readDocMessagePlaceholderFragment(ctx context.Context, plan DocMessagePlaceholderPlan, cursor DocMessagePlaceholderCursor, load func(context.Context, ContentHash) ([]byte, error), maxBytes int) (result DocMessagePlaceholderFragment, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocMessagePlaceholderFragment{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMessagePlaceholderFragment{}, err
	}
	source, err := plan.ProjectionSource()
	if err != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.From < 0 || cursor.From >= plan.TextBytes || load == nil || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes {
		return DocMessagePlaceholderFragment{}, ErrIntegrity
	}
	typeBytes, nameBytes := 0, 0
	if plan.Type != nil {
		typeBytes = plan.Type.TextBytes
	}
	if plan.Name != nil {
		nameBytes = plan.Name.TextBytes
	}
	separator := ""
	if nameBytes > 0 {
		separator = ": "
	}
	parts := []struct {
		text       string
		generation *DocDecodedTextGeneration
		length     int
	}{
		{text: "[", length: 1},
		{generation: plan.Type, length: typeBytes},
		{text: separator, length: len(separator)},
		{generation: plan.Name, length: nameBytes},
		{text: "]", length: 1},
	}
	limit := min(maxBytes, plan.TextBytes-cursor.From)
	output := make([]byte, 0, limit)
	position := 0
	for _, part := range parts {
		if len(output) == limit {
			break
		}
		start := max(cursor.From-position, 0)
		position += part.length
		if start >= part.length {
			continue
		}
		count := min(part.length-start, limit-len(output))
		if part.generation == nil {
			output = append(output, part.text[start:start+count]...)
			continue
		}
		reader, err := newDocDecodedTextReader(*part.generation, load)
		if err != nil {
			return DocMessagePlaceholderFragment{}, err
		}
		for consumed := 0; consumed < count; {
			read := min(4096, count-consumed)
			raw, err := reader.readBytes(ctx, start+consumed, read)
			if err != nil {
				return DocMessagePlaceholderFragment{}, err
			}
			output = append(output, raw...)
			consumed += read
		}
	}
	if len(output) != limit || len(output) == 0 || !utf8.RuneStart(output[0]) {
		return DocMessagePlaceholderFragment{}, ErrIntegrity
	}
	for !utf8.Valid(output) && len(output) > 0 {
		output = output[:len(output)-1]
	}
	if len(output) == 0 || limit-len(output) > utf8.UTFMax-1 {
		return DocMessagePlaceholderFragment{}, ErrIntegrity
	}
	next := cursor
	next.From += len(output)
	return DocMessagePlaceholderFragment{text: string(output), from: cursor.From, anchor: plan.Block.Anchor, next: next}, nil
}
