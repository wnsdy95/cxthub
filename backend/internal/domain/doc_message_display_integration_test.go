package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type messageDisplayFixture struct {
	plan          DocMessageDisplayPlan
	objects       map[ContentHash][]byte
	steps         int
	maxCheckpoint int
	maxLoads      int
}

func messageDisplayChildren(test *testing.T, raw []byte, inputs []DocMessageInput, objects map[ContentHash][]byte) []DocMessageDisplayBlockPlan {
	test.Helper()
	children := make([]DocMessageDisplayBlockPlan, len(inputs))
	merge := func(prepared map[ContentHash][]byte) {
		for hash, body := range prepared {
			objects[hash] = bytes.Clone(body)
		}
	}
	for index, input := range inputs {
		if input.IsText() {
			source, err := input.TextSource()
			if err != nil {
				test.Fatal(err)
			}
			if source == nil {
				continue
			}
			prepared := buildDecodedMarkdownInputFixture(test, buildDecodedTextSourceFixture(test, raw, *source))
			children[index].Markdown = &prepared.plan
			merge(prepared.decoded.objects)
			continue
		}
		blockType, name, err := input.PlaceholderSources()
		if err != nil {
			test.Fatal(err)
		}
		var decoded [2]*DocDecodedTextGeneration
		for position, source := range []*DocDecodedTextSource{blockType, name} {
			if source == nil {
				continue
			}
			prepared := buildDecodedTextSourceFixture(test, raw, *source)
			decoded[position] = &prepared.generation
			merge(prepared.objects)
		}
		plan, err := NewDocMessagePlaceholderPlan(input, decoded[0], decoded[1])
		if err != nil {
			test.Fatal(err)
		}
		children[index].Placeholder = &plan
	}
	return children
}

func buildMessageDisplayFixture(test *testing.T, raw []byte) messageDisplayFixture {
	test.Helper()
	message := buildMessageInputFixture(test, raw)
	inputs := messageFixtureInputs(test, message)
	children := messageDisplayChildren(test, raw, inputs, message.objects)
	source := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: message.generation}
	state, err := NewDocMessageDisplayState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := messageDisplayFixture{objects: message.objects}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := fixture.objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Done(source) {
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		loads := 0
		step, err := StepDocMessageDisplayPlan(context.Background(), source, state, children[state.Next:min(state.Next+DocMessageDisplayStepBlocks, len(children))], func(ctx context.Context, hash ContentHash) ([]byte, error) { loads++; return load(ctx, hash) })
		if err != nil || step.Processed <= 0 || step.Processed > DocMessageDisplayStepBlocks || step.State.Next != state.Next+step.Processed || loads > DocDisplayMappedReplayObjects {
			test.Fatal("message display plan failed bounded progress", state.Next, step.Processed, loads, err)
		}
		fixture.steps++
		fixture.maxLoads = max(fixture.maxLoads, loads)
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("message plan mutated previous checkpoint", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) > MaxDocMessageDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid message-plan object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(source)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("invalid or body-retaining message plan checkpoint", err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		state, err = DecodeDocMessageDisplayState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
		if err := ValidateDocMessageDisplayCheckpoint(context.Background(), source, state, load); err != nil {
			test.Fatal("message checkpoint failed restart", err)
		}
	}
	plan, produced, err := FinishDocMessageDisplayPlan(context.Background(), source, state, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range produced {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.plan, err = DecodeDocMessageDisplayPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func TestDocMessageDisplayBatchReadBudgetAndAtomicFailure(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{"type":"text","text":"**a**"},`, 39) + `{"type":"text","text":"**a**"}]}`)
	fixture := buildMessageDisplayFixture(test, raw)
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	before := bytes.Clone(input.initial)
	loads := 0
	page, next, err := input.read(context.Background(), input.initial, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return fixture.objects[hash], nil
	})
	if err != nil || loads != DocDisplayMappedReplayObjects || len(page.Text) <= 1 || next == nil || !bytes.Equal(before, input.initial) {
		test.Fatal("message batch did not yield bounded multi-part progress", page.Text, loads, err)
	}
	for _, cancelRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		failed, cursor, err := input.read(ctx, input.initial, DocDisplayPageTextBytes, DocDisplayPageRuns, func(_ context.Context, hash ContentHash) ([]byte, error) {
			calls++
			if calls == 30 {
				if cancelRead {
					cancel()
				} else {
					return nil, ErrNotFound
				}
			}
			return fixture.objects[hash], nil
		})
		cancel()
		want := ErrNotFound
		if cancelRead {
			want = context.Canceled
		}
		if !errors.Is(err, want) || !reflect.DeepEqual(failed, DocDisplayPage{}) || cursor != nil || !bytes.Equal(before, input.initial) {
			test.Fatal("partially produced batch survived a fatal error", cancelRead, failed.Text, err)
		}
	}
}
