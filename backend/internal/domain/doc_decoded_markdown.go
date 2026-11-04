package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

type DocDecodedMarkdownPlan struct {
	Version  int                      `json:"version"`
	Input    DocDecodedTextGeneration `json:"input"`
	Markdown DocMarkdownPlan          `json:"markdown"`
}

func StepDocDecodedMarkdownPlan(ctx context.Context, input DocDecodedTextGeneration, state DocMarkdownPlanState, loadObject func(context.Context, ContentHash) ([]byte, error)) (DocMarkdownPlanStep, error) {
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlanStep{}, err
	}
	if state.Input.Hash != input.Hash || state.Input.Bytes != input.TextBytes {
		return DocMarkdownPlanStep{}, ErrIntegrity
	}
	reader, err := newDocDecodedTextReader(input, loadObject)
	if err != nil {
		return DocMarkdownPlanStep{}, err
	}
	return StepDocMarkdownPlan(ctx, state, reader.readBytes)
}

func (plan DocDecodedMarkdownPlan) Validate() error {
	if plan.Version != 1 || plan.Input.Validate() != nil || plan.Markdown.Validate() != nil || plan.Markdown.Input.Hash != plan.Input.Hash || plan.Markdown.Input.Bytes != plan.Input.TextBytes {
		return ErrIntegrity
	}
	return nil
}

func (plan DocDecodedMarkdownPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDecodedMarkdownPlan(expected ContentHash, raw []byte) (DocDecodedMarkdownPlan, error) {
	var plan DocDecodedMarkdownPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocDecodedMarkdownPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDecodedMarkdownPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan DocDecodedMarkdownPlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Input.Source.EventHash, EventBytes: plan.Input.Source.EventBytes, Plan: HashContent(raw), TextBytes: plan.Markdown.TextBytes}
	return source, source.Validate()
}

type DocDecodedMarkdownCursor struct {
	Version int                   `json:"version"`
	Plan    ContentHash           `json:"plan"`
	Render  DocMarkdownPlanCursor `json:"render"`
}

func NewDocDecodedMarkdownCursor(plan DocDecodedMarkdownPlan) (DocDecodedMarkdownCursor, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDecodedMarkdownCursor{}, err
	}
	render, err := NewDocMarkdownPlanCursor(plan.Markdown)
	if err != nil {
		return DocDecodedMarkdownCursor{}, err
	}
	return DocDecodedMarkdownCursor{Version: 1, Plan: source.Plan, Render: render}, nil
}

type DocDecodedMarkdownFragment struct {
	display DocMarkdownPlanFragment
	input   DocDecodedTextGeneration
	next    DocDecodedMarkdownCursor
}

func (fragment DocDecodedMarkdownFragment) Text() string { return fragment.display.Text() }
func (fragment DocDecodedMarkdownFragment) From() int    { return fragment.display.From() }
func (fragment DocDecodedMarkdownFragment) Next() DocDecodedMarkdownCursor {
	return fragment.next
}

func (fragment DocDecodedMarkdownFragment) LocateSource(ctx context.Context, offset, length int, loadObject func(context.Context, ContentHash) ([]byte, error)) (DocFindSpan, error) {
	if err := ctx.Err(); err != nil {
		return DocFindSpan{}, err
	}
	span, err := fragment.display.SourceRange(offset, length)
	if err != nil {
		return DocFindSpan{}, err
	}
	return LocateDocDecodedTextSource(ctx, fragment.input, span.From, span.To-span.From, loadObject)
}

func ReadDocDecodedMarkdownFragment(ctx context.Context, plan DocDecodedMarkdownPlan, cursor DocDecodedMarkdownCursor, loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocDecodedMarkdownFragment, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDecodedMarkdownFragment{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDecodedMarkdownFragment{}, err
	}
	source, err := plan.ProjectionSource()
	if err != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Render.Validate(plan.Markdown) != nil || loadObject == nil {
		return DocDecodedMarkdownFragment{}, ErrIntegrity
	}
	reader, err := newDocDecodedTextReader(plan.Input, loadObject)
	if err != nil {
		return DocDecodedMarkdownFragment{}, err
	}
	display, err := ReadDocMarkdownPlanFragment(ctx, plan.Markdown, cursor.Render, loadObject, reader.readBytes)
	if err != nil {
		return DocDecodedMarkdownFragment{}, err
	}
	next := cursor
	next.Render = display.Next()
	return DocDecodedMarkdownFragment{display: display, input: plan.Input, next: next}, nil
}
