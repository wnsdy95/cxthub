package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDocDisplayCheckpointVerifiesLastStagedPage(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("a", 100_000) + `"}`)
	ctx := context.Background()
	source, err := BuildDocDisplayStringSourceReader(ctx, HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocDisplayString(ctx, source, state, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	for _, object := range step.Objects {
		objects[object.Hash] = object.Bytes
	}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) {
		body, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return body, nil
	}
	if err := ValidateDocDisplayStringCheckpoint(ctx, source, step.State, load); err != nil {
		test.Fatal("valid checkpoint rejected", err)
	}
	for _, completed := range []bool{false, true} {
		forged := step.State
		forged.Done = completed
		forged.Next = source.Token.To - 1
		if completed {
			forged.Next++
		}
		if err := forged.Validate(source); err != nil {
			test.Fatal("test needs a syntactically plausible checkpoint", err)
		}
		if err := ValidateDocDisplayStringCheckpoint(ctx, source, forged, load); !errors.Is(err, ErrIntegrity) {
			test.Fatal("checkpoint skipped staged source bytes", completed, err)
		}
	}
	delete(objects, step.Objects[len(step.Objects)-1].Hash)
	if err := ValidateDocDisplayStringCheckpoint(ctx, source, step.State, load); !errors.Is(err, ErrNotFound) {
		test.Fatal("missing staged page was accepted", err)
	}
}
