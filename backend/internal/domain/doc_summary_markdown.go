package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

type DocSummaryMarkdownPlan struct {
	Version  int                      `json:"version"`
	Input    DocSummaryTextGeneration `json:"input"`
	Markdown DocMarkdownPlan          `json:"markdown"`
}

func StepDocSummaryMarkdownPlan(ctx context.Context, input DocSummaryTextGeneration, state DocMarkdownPlanState, load func(context.Context, ContentHash) ([]byte, error)) (DocMarkdownPlanStep, error) {
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlanStep{}, err
	}
	if state.Input.Hash != input.Hash || state.Input.Bytes != input.TextBytes {
		return DocMarkdownPlanStep{}, ErrIntegrity
	}
	reader, err := newDocSummaryTextReader(input, load)
	if err != nil {
		return DocMarkdownPlanStep{}, err
	}
	return StepDocMarkdownPlan(ctx, state, reader.readBytes)
}

func (plan DocSummaryMarkdownPlan) Validate() error {
	if plan.Version != 1 || plan.Input.Validate() != nil || plan.Markdown.Validate() != nil || plan.Markdown.Input.Hash != plan.Input.Hash || plan.Markdown.Input.Bytes != plan.Input.TextBytes {
		return ErrIntegrity
	}
	return nil
}

func (plan DocSummaryMarkdownPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocSummaryMarkdownPlan(expected ContentHash, raw []byte) (DocSummaryMarkdownPlan, error) {
	var plan DocSummaryMarkdownPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocSummaryMarkdownPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocSummaryMarkdownPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan DocSummaryMarkdownPlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Input.Source.Message.Source.EventHash, EventBytes: plan.Input.Source.Message.Source.EventBytes, Plan: HashContent(raw), TextBytes: plan.Markdown.TextBytes}
	return source, source.Validate()
}

type DocSummaryMarkdownCursor struct {
	Version int                   `json:"version"`
	Plan    ContentHash           `json:"plan"`
	Render  DocMarkdownPlanCursor `json:"render"`
}

func NewDocSummaryMarkdownCursor(plan DocSummaryMarkdownPlan) (DocSummaryMarkdownCursor, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocSummaryMarkdownCursor{}, err
	}
	render, err := NewDocMarkdownPlanCursor(plan.Markdown)
	if err != nil {
		return DocSummaryMarkdownCursor{}, err
	}
	return DocSummaryMarkdownCursor{Version: 1, Plan: source.Plan, Render: render}, nil
}

type DocSummaryMarkdownFragment struct {
	display DocMarkdownPlanFragment
	input   DocSummaryTextGeneration
	next    DocSummaryMarkdownCursor
}

func (fragment DocSummaryMarkdownFragment) Text() string { return fragment.display.Text() }
func (fragment DocSummaryMarkdownFragment) From() int    { return fragment.display.From() }
func (fragment DocSummaryMarkdownFragment) Next() DocSummaryMarkdownCursor {
	return fragment.next
}

func (fragment DocSummaryMarkdownFragment) LocateSource(ctx context.Context, offset, length int, load func(context.Context, ContentHash) ([]byte, error)) (DocFindSpan, error) {
	if err := ctx.Err(); err != nil {
		return DocFindSpan{}, err
	}
	span, err := fragment.display.SourceRange(offset, length)
	if err != nil {
		return DocFindSpan{}, err
	}
	return LocateDocSummaryTextSource(ctx, fragment.input, span.From, span.To-span.From, load)
}

func ReadDocSummaryMarkdownFragment(ctx context.Context, plan DocSummaryMarkdownPlan, cursor DocSummaryMarkdownCursor, load func(context.Context, ContentHash) ([]byte, error)) (result DocSummaryMarkdownFragment, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocSummaryMarkdownFragment{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocSummaryMarkdownFragment{}, err
	}
	source, err := plan.ProjectionSource()
	if err != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Render.Validate(plan.Markdown) != nil || load == nil {
		return DocSummaryMarkdownFragment{}, ErrIntegrity
	}
	reader, err := newDocSummaryTextReader(plan.Input, load)
	if err != nil {
		return DocSummaryMarkdownFragment{}, err
	}
	display, err := ReadDocMarkdownPlanFragment(ctx, plan.Markdown, cursor.Render, load, reader.readBytes)
	if err != nil {
		return DocSummaryMarkdownFragment{}, err
	}
	next := cursor
	next.Render = display.Next()
	return DocSummaryMarkdownFragment{display: display, input: plan.Input, next: next}, nil
}
