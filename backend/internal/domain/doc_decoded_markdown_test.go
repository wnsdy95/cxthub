package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

type decodedMarkdownFixture struct {
	decoded decodedTextFixture
	plan    DocDecodedMarkdownPlan
	steps   int
	loads   int
}

func buildDecodedMarkdownFixture(test *testing.T, raw []byte) decodedMarkdownFixture {
	test.Helper()
	decoded := buildDecodedTextFixture(test, raw)
	return buildDecodedMarkdownInputFixture(test, decoded)
}

func buildDecodedMarkdownInputFixture(test *testing.T, decoded decodedTextFixture) decodedMarkdownFixture {
	test.Helper()
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: decoded.generation.Hash, Bytes: decoded.generation.TextBytes})
	if err != nil {
		test.Fatal(err)
	}
	fixture := decodedMarkdownFixture{decoded: decoded}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		fixture.loads++
		body, found := fixture.decoded.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Done() {
		fixture.steps++
		step, err := StepDocDecodedMarkdownPlan(context.Background(), decoded.generation, state, load)
		if err != nil {
			test.Fatal("Markdown from verified decoded pages failed", fixture.steps, err)
		}
		for _, object := range step.Objects {
			fixture.decoded.objects[object.Hash] = bytes.Clone(object.Bytes)
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
	plan, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.decoded.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan = DocDecodedMarkdownPlan{Version: 1, Input: decoded.generation, Markdown: plan}
	if err := fixture.plan.Validate(); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestDocDecodedMarkdownCursorBindsOriginEvenWhenTextAndPlanAreReusable(test *testing.T) {
	first := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**same**","opaque":"A"}`))
	second := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","opaque":"different origin","redacted_summary":"**same**"}`))
	if first.plan.Markdown != second.plan.Markdown {
		test.Fatal("identical decoded text should share a Markdown plan")
	}
	cursor, err := NewDocDecodedMarkdownCursor(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	called := false
	fragment, err := ReadDocDecodedMarkdownFragment(context.Background(), second.plan, cursor,
		func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocDecodedMarkdownFragment{}) {
		test.Fatal("cursor switched original event", called, err)
	}
	encoded, err := first.plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocDecodedMarkdownPlan(HashContent(encoded), encoded); err != nil || decoded != first.plan {
		test.Fatal("bridge plan codec failed", err)
	}
	projection, err := first.plan.ProjectionSource()
	if err != nil || projection.EventHash != first.decoded.source.EventHash || projection.TextBytes != first.plan.Markdown.TextBytes || projection.Plan != HashContent(encoded) {
		test.Fatal("projection lost original binding", projection, err)
	}
}
