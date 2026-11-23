package domain

import (
	"bytes"
	"context"
	"testing"
)

type displayAssemblyFixture struct {
	generation DocDisplayProjectionGeneration
	objects    map[ContentHash][]byte
	steps      int
	maxState   int
	maxLoads   int
}

func assembleDisplayFixture(test *testing.T, input DocDisplayAssemblyInput, objects map[ContentHash][]byte) displayAssemblyFixture {
	test.Helper()
	state, err := NewDocDisplayAssemblyState(input)
	if err != nil {
		test.Fatal(err)
	}
	fixture := displayAssemblyFixture{objects: objects}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	for !state.Output.Done {
		before, err := state.Encode(input)
		if err != nil {
			test.Fatal(err)
		}
		loads := 0
		step, err := StepDocDisplayAssembly(context.Background(), input, state, func(ctx context.Context, hash ContentHash) ([]byte, error) { loads++; return load(ctx, hash) })
		if err != nil {
			test.Fatal("real assembly failed", fixture.steps, state.Output.TextBytes, state.Pending, err)
		}
		fixture.steps++
		fixture.maxLoads = max(fixture.maxLoads, loads)
		after, err := state.Encode(input)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("assembly changed previous checkpoint", err)
		}
		for _, object := range step.Objects {
			if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocDisplayPageBytes || HashContent(object.Bytes) != object.Hash {
				test.Fatal("bad assembled staging object")
			}
			if existing, found := objects[object.Hash]; found && !bytes.Equal(existing, object.Bytes) {
				test.Fatal("immutable assembly object changed")
			}
			objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		encoded, err := step.State.Encode(input)
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) || bytes.Contains(encoded, []byte("PUBLIC_BODY_MARKER")) {
			test.Fatal("assembly checkpoint failed to progress or retained text", err)
		}
		fixture.maxState = max(fixture.maxState, len(encoded))
		state, err = DecodeDocDisplayAssemblyState(input, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal(err)
		}
		if err := ValidateDocDisplayAssemblyCheckpoint(context.Background(), input, state, load); err != nil {
			test.Fatal("real assembled checkpoint invalid", err)
		}
	}
	generation, produced, err := FinishDocDisplayAssembly(context.Background(), input, state, load)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range produced {
		objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.generation, err = DecodeDocDisplayProjectionGeneration(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}
