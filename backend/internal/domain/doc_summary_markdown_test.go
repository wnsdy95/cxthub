package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

type summaryMarkdownFixture struct {
	input summaryTextFixture
	plan  DocSummaryMarkdownPlan
	steps int
	loads int
}

func buildSummaryMarkdownFixture(test *testing.T, raw []byte) summaryMarkdownFixture {
	test.Helper()
	fixture := summaryMarkdownFixture{input: buildSummaryTextFixture(test, raw)}
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: fixture.input.generation.Hash, Bytes: fixture.input.generation.TextBytes})
	if err != nil {
		test.Fatal(err)
	}
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		fixture.loads++
		return summaryFixtureLoad(fixture.input)(ctx, hash)
	}
	for !state.Done() {
		step, err := StepDocSummaryMarkdownPlan(context.Background(), fixture.input.generation, state, load)
		if err != nil {
			test.Fatal("summary Markdown compile failed", fixture.steps, err)
		}
		fixture.steps++
		for _, object := range step.Objects {
			fixture.input.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode()
		if err != nil {
			test.Fatal(err)
		}
		state, err = DecodeDocMarkdownPlanState(HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
	}
	markdown, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.input.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan = DocSummaryMarkdownPlan{Version: 1, Input: fixture.input.generation, Markdown: markdown}
	encoded, err := fixture.plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.plan, err = DecodeDocSummaryMarkdownPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestDocSummaryMarkdownCursorCannotSwitchOriginalMessage(test *testing.T) {
	first := buildSummaryMarkdownFixture(test, []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"**same**"}]}`))
	second := buildSummaryMarkdownFixture(test, []byte(`{"kind":"message","compact_summary":true,"opaque":true,"blocks":[{"text":"**same**"}]}`))
	if first.plan.Markdown != second.plan.Markdown {
		test.Fatal("same joined input should share Markdown plan")
	}
	cursor, err := NewDocSummaryMarkdownCursor(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocSummaryMarkdownFragment(context.Background(), second.plan, cursor, func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocSummaryMarkdownFragment{}) {
		test.Fatal("summary cursor switched source message", called, err)
	}
}
