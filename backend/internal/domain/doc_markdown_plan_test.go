package domain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type markdownPlanFixture struct {
	plan          DocMarkdownPlan
	state         DocMarkdownPlanState
	objects       map[ContentHash][]byte
	steps         int
	readBytes     int
	maxCheckpoint int
}

func compileMarkdownFixture(test testing.TB, source string, resume bool) markdownPlanFixture {
	test.Helper()
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	fixture := markdownPlanFixture{objects: make(map[ContentHash][]byte)}
	for state.Phase != docMarkdownPlanDone {
		fixture.steps++
		if fixture.steps > 100000 {
			test.Fatal("compiler did not complete")
		}
		before, err := state.Encode()
		if err != nil {
			test.Fatal("invalid before", state.Phase, err)
		}
		reads := 0
		step, err := StepDocMarkdownPlan(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			if from < 0 || length <= 0 || length > 4096 || from > len(source)-length {
				test.Fatalf("unbounded source request: %d+%d/%d", from, length, len(source))
			}
			reads += length
			return []byte(source[from : from+length]), nil
		})
		if err != nil {
			test.Fatalf("step %d phase %d cursor %d line=%+v lookup=%d/%d copy=%d..%d: %v", fixture.steps, state.Phase, state.InlineCursor, state.Line, state.LookupKind, state.LookupNext, state.CopyFrom, state.CopyTo, err)
		}
		if step.ReadBytes != reads || reads > DocDisplayStepSourceBytes || step.State.OperationCount-state.OperationCount > DocMarkdownPlanStepOperations {
			test.Fatal("step exceeded work bounds", step.ReadBytes, reads, step.State.OperationCount-state.OperationCount)
		}
		after, err := state.Encode()
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("input checkpoint mutated", err)
		}
		encoded, err := step.State.Encode()
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(before, encoded) {
			test.Fatal("checkpoint unbounded or no progress", len(encoded), err)
		}
		fixture.maxCheckpoint = max(fixture.maxCheckpoint, len(encoded))
		fixture.readBytes += reads
		for _, object := range step.Objects {
			if len(object.Bytes) == 0 || len(object.Bytes) > max(MaxDocMarkdownPlanPageBytes, MaxDocRangeDirectoryBytes) || HashContent(object.Bytes) != object.Hash {
				test.Fatal("invalid staged object")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		state = step.State
		if resume {
			state, err = DecodeDocMarkdownPlanState(HashContent(encoded), encoded)
			if err != nil {
				test.Fatal("checkpoint cannot resume", err)
			}
		}
	}
	plan, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan, fixture.state = plan, state
	return fixture
}

func TestDocMarkdownPlanManyBlocksAndSelectedPlanPage(test *testing.T) {
	source := strings.Repeat("# A **B** C\n", 5000)
	compiled := compileMarkdownFixture(test, source, true)
	if compiled.plan.Pages.Count < 129 {
		test.Fatal("fixture did not cross directory fanout", compiled.plan.Pages.Count)
	}
	loads := 0
	page, err := ReadDocMarkdownPlanPage(context.Background(), compiled.plan, compiled.plan.Pages.Count-1, func(_ context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return compiled.objects[hash], nil
	})
	if err != nil || len(page.Operations) == 0 || loads > 3 {
		test.Fatal("selected page loaded unrelated history", loads, err)
	}
	encoded, err := compiled.plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if decoded, err := DecodeDocMarkdownPlan(HashContent(encoded), encoded); err != nil || decoded != compiled.plan {
		test.Fatal(decoded, err)
	}
	test.Logf("pages=%d operations=%d steps=%d selected_loads=%d checkpoint_bytes=%d", compiled.plan.Pages.Count, compiled.plan.OperationCount, compiled.steps, loads, compiled.maxCheckpoint)
}

func TestDocMarkdownPlanHashMismatchIOAndCancellationReturnNoEffects(test *testing.T) {
	source := "# body"
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		load func(context.Context, int, int) ([]byte, error)
		want error
	}{
		{"hash mismatch", func(_ context.Context, from, length int) ([]byte, error) {
			return []byte("# fake")[from : from+length], nil
		}, ErrIntegrity},
		{"invalid UTF8", func(_ context.Context, from, length int) ([]byte, error) {
			return []byte("# \xffody")[from : from+length], nil
		}, ErrIntegrity},
		{"short", func(context.Context, int, int) ([]byte, error) { return []byte("#"), nil }, ErrIntegrity},
		{"IO error", func(context.Context, int, int) ([]byte, error) { return nil, io.ErrClosedPipe }, io.ErrClosedPipe},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			got, err := StepDocMarkdownPlan(context.Background(), state, scenario.load)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(got, DocMarkdownPlanStep{}) {
				test.Fatal("failed step exposed effects", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	got, err := StepDocMarkdownPlan(ctx, state, func(_ context.Context, from, length int) ([]byte, error) {
		called = true
		cancel()
		return []byte(source[from : from+length]), nil
	})
	if !called || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocMarkdownPlanStep{}) {
		test.Fatal("cancellation exposed effects", got, err)
	}
	if plan, objects, err := FinishDocMarkdownPlan(context.Background(), state); !errors.Is(err, ErrIntegrity) || plan != (DocMarkdownPlan{}) || objects != nil {
		test.Fatal("unfinished plan published", plan, objects, err)
	}
}
