package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

type markdownReviewCompilation struct {
	plan      DocMarkdownPlan
	state     DocMarkdownPlanState
	objects   map[ContentHash][]byte
	steps     int
	readBytes int
}

func markdownReviewCompile(test *testing.T, source string, maxSteps int) markdownReviewCompilation {
	test.Helper()
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	compiled := markdownReviewCompilation{objects: make(map[ContentHash][]byte)}
	scratch := make([]byte, 4096)
	for state.Phase != docMarkdownPlanDone {
		compiled.steps++
		if compiled.steps > maxSteps {
			test.Fatalf("compiler exceeded %d steps for %d bytes", maxSteps, len(source))
		}
		prior, err := state.Encode()
		if err != nil {
			test.Fatal(err)
		}
		loaded := 0
		step, err := StepDocMarkdownPlan(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			if from < 0 || length <= 0 || length > len(scratch) || from > len(source)-length {
				test.Fatalf("source request outside bounds: %d+%d/%d", from, length, len(source))
			}
			loaded += length
			copy(scratch, source[from:from+length])
			return scratch[:length], nil
		})
		if err != nil {
			test.Fatalf("step %d phase %d cursor %d lookup %d/%d: %v", compiled.steps, state.Phase, state.InlineCursor, state.LookupKind, state.LookupNext, err)
		}
		unchanged, err := state.Encode()
		if err != nil || !bytes.Equal(prior, unchanged) {
			test.Fatal("step mutated its caller-owned checkpoint", err)
		}
		if step.ReadBytes != loaded || loaded > DocDisplayStepSourceBytes || step.State.OperationCount-state.OperationCount > DocMarkdownPlanStepOperations {
			test.Fatal("step exceeded source/operation budget", loaded, step.ReadBytes, step.State.OperationCount-state.OperationCount)
		}
		encoded, err := step.State.Encode()
		if err != nil || len(encoded) > MaxDocDisplayStateBytes || bytes.Equal(prior, encoded) {
			test.Fatal("invalid or unchanged checkpoint", err)
		}
		if step.State.Digest == "" {
			hashState := step.State.HashState
			if len(hashState) != 4+sha256.Size+sha256.BlockSize+8 || step.State.HashNext%sha256.BlockSize != 0 || binary.BigEndian.Uint64(hashState[len(hashState)-8:]) != uint64(step.State.HashNext) || !bytes.Equal(hashState[4+sha256.Size:len(hashState)-8], make([]byte, sha256.BlockSize)) {
				test.Fatal("checkpoint retained a SHA plaintext tail or inconsistent hash count")
			}
		}
		for _, object := range step.Objects {
			if HashContent(object.Bytes) != object.Hash || len(object.Bytes) > max(MaxDocMarkdownPlanPageBytes, MaxDocRangeDirectoryBytes) {
				test.Fatal("invalid immutable object")
			}
			compiled.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		compiled.readBytes += loaded
		state, err = DecodeDocMarkdownPlanState(HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("checkpoint restart failed", err)
		}
	}
	plan, objects, err := FinishDocMarkdownPlan(context.Background(), state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		compiled.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	compiled.plan, compiled.state = plan, state
	return compiled
}

func TestDocMarkdownPlanReviewHashCheckpointPrivacyAndIntegrity(test *testing.T) {
	source := strings.Repeat("private-tail-marker-🙂", 10000)
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	for stepIndex := 0; stepIndex < 3; stepIndex++ {
		step, err := StepDocMarkdownPlan(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			return []byte(source[from : from+length]), nil
		})
		if err != nil {
			test.Fatal(err)
		}
		state = step.State
		if state.Digest != "" || state.HashNext == 0 {
			test.Fatal("fixture did not retain an intermediate SHA checkpoint")
		}
		digest := sha256.New()
		_, _ = digest.Write([]byte(source[:state.HashNext]))
		want, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil || !bytes.Equal(want, state.HashState) {
			test.Fatal("resumed SHA checkpoint diverged from the exact source prefix", err)
		}
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*DocMarkdownPlanState)
	}{
		{"plaintext tail", func(value *DocMarkdownPlanState) { value.HashState[4+sha256.Size] = 's' }},
		{"unaligned count", func(value *DocMarkdownPlanState) { value.HashNext++ }},
		{"wrong encoded count", func(value *DocMarkdownPlanState) { value.HashState[len(value.HashState)-1] ^= 64 }},
		{"wrong format", func(value *DocMarkdownPlanState) { value.HashState[3]++ }},
		{"short hash state", func(value *DocMarkdownPlanState) { value.HashState = value.HashState[:len(value.HashState)-1] }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			changed := state
			changed.HashState = bytes.Clone(state.HashState)
			scenario.mutate(&changed)
			if !errors.Is(changed.Validate(), ErrIntegrity) {
				test.Fatal("malformed SHA checkpoint accepted")
			}
			raw, err := canonicalJSON(changed)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := DecodeDocMarkdownPlanState(HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("rehashed malformed SHA checkpoint accepted", err)
			}
		})
	}
}

func TestDocMarkdownPlanReviewOverflowAndSourceBounds(test *testing.T) {
	input := DocMarkdownPlanInput{Hash: HashContent([]byte("abcdef")), Bytes: 6}
	for _, bound := range []int{math.MinInt, -1, MaxConversationDocumentBytes + 1, math.MaxInt} {
		changed := input
		changed.Bytes = bound
		if !errors.Is(changed.Validate(), ErrIntegrity) {
			test.Fatal("invalid input length accepted", bound)
		}
	}
	for _, operation := range []DocMarkdownPlanOperation{
		{From: math.MinInt, To: 1}, {From: -1, To: 1}, {From: 0, To: 0},
		{From: 2, To: 1}, {From: 0, To: 7}, {From: 0, To: math.MaxInt},
		{From: math.MaxInt - 1, To: math.MaxInt}, {From: 0, To: 1, Text: "private content"},
		{From: 0, To: 1, Text: "\t"}, {From: 0, To: 1, Text: "\n\n\n"},
	} {
		if !errors.Is(operation.Validate(input), ErrIntegrity) {
			test.Fatal("invalid source operation accepted", operation)
		}
		page := DocMarkdownPlanPage{Version: 1, Input: input.identity(), Operations: []DocMarkdownPlanOperation{operation}}
		raw, err := canonicalJSON(page)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := DecodeDocMarkdownPlanPage(input, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("rehashed out-of-source operation accepted", operation, err)
		}
	}
	source := strings.Repeat("x", 80000)
	initial, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocMarkdownPlan(context.Background(), initial, func(_ context.Context, from, length int) ([]byte, error) {
		return []byte(source[from : from+length]), nil
	})
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocMarkdownPlanState){
		"scanned":       func(value *DocMarkdownPlanState) { value.Scanned = math.MaxInt },
		"hash count":    func(value *DocMarkdownPlanState) { value.HashNext = math.MaxInt },
		"line cursor":   func(value *DocMarkdownPlanState) { value.Line.Next = math.MaxInt },
		"block end":     func(value *DocMarkdownPlanState) { value.Block.To = math.MaxInt },
		"inline cursor": func(value *DocMarkdownPlanState) { value.InlineCursor = math.MaxInt },
		"copy end":      func(value *DocMarkdownPlanState) { value.CopyTo = math.MaxInt },
		"space end":     func(value *DocMarkdownPlanState) { value.PendingSpaceTo = math.MaxInt },
		"delimiter":     func(value *DocMarkdownPlanState) { value.NextDelimiter[0] = math.MaxInt },
		"negative cache": func(value *DocMarkdownPlanState) {
			value.NextDelimiter[0] = -2
		},
		"operation count": func(value *DocMarkdownPlanState) { value.OperationCount = math.MaxInt },
		"directory count": func(value *DocMarkdownPlanState) { value.Directory.Count = math.MaxInt },
		"output bytes":    func(value *DocMarkdownPlanState) { value.TextBytes = math.MaxInt },
		"page offset":     func(value *DocMarkdownPlanState) { value.PageFrom = math.MaxInt },
	} {
		test.Run(name, func(test *testing.T) {
			changed := step.State
			mutate(&changed)
			if !errors.Is(changed.Validate(), ErrIntegrity) {
				test.Fatal("overflowing checkpoint accepted")
			}
			got, err := StepDocMarkdownPlan(context.Background(), changed, func(context.Context, int, int) ([]byte, error) {
				test.Fatal("invalid checkpoint reached the source reader")
				return nil, nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocMarkdownPlanStep{}) {
				test.Fatal("invalid checkpoint exposed effects", err)
			}
		})
	}
}

func TestDocMarkdownPlanReviewCanonicalMetadataAndPageCounts(test *testing.T) {
	compiled := markdownReviewCompile(test, "before **visible** after", 8)
	page, err := ReadDocMarkdownPlanPage(context.Background(), compiled.plan, 0, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return compiled.objects[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		value  any
		decode func(ContentHash, []byte) error
	}{
		{"plan", compiled.plan, func(hash ContentHash, raw []byte) error { _, err := DecodeDocMarkdownPlan(hash, raw); return err }},
		{"state", compiled.state, func(hash ContentHash, raw []byte) error { _, err := DecodeDocMarkdownPlanState(hash, raw); return err }},
		{"page", page, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMarkdownPlanPage(compiled.plan.Input, hash, raw)
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw, err := canonicalJSON(scenario.value)
			if err != nil || scenario.decode(HashContent(raw), raw) != nil {
				test.Fatal("valid canonical metadata rejected", err)
			}
			for _, changed := range [][]byte{
				append(bytes.Clone(raw), ' '),
				append([]byte(`{"unknown":true,`), raw[1:]...),
				append([]byte(`{"version":1,`), raw[1:]...),
				bytes.Replace(raw, []byte(`"version"`), []byte(`"\u0076ersion"`), 1),
			} {
				if err := scenario.decode(HashContent(changed), changed); !errors.Is(err, ErrIntegrity) {
					test.Fatal("noncanonical metadata accepted after rehashing", err)
				}
			}
			if err := scenario.decode(HashContent([]byte("wrong object")), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("wrong object hash accepted", err)
			}
		})
	}
	for name, mutate := range map[string]func(*DocMarkdownPlan){
		"extra operation": func(value *DocMarkdownPlan) { value.OperationCount++ },
		"extra byte":      func(value *DocMarkdownPlan) { value.TextBytes++ },
		"missing byte":    func(value *DocMarkdownPlan) { value.TextBytes-- },
		"count overflow":  func(value *DocMarkdownPlan) { value.OperationCount = math.MaxInt },
		"length overflow": func(value *DocMarkdownPlan) { value.TextBytes = math.MaxInt },
		"directory count": func(value *DocMarkdownPlan) { value.Pages.Count++ },
	} {
		test.Run(name, func(test *testing.T) {
			changed := compiled.plan
			mutate(&changed)
			got, err := ReadDocMarkdownPlanPage(context.Background(), changed, 0, func(_ context.Context, hash ContentHash) ([]byte, error) {
				return compiled.objects[hash], nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, DocMarkdownPlanPage{}) {
				test.Fatal("selected page accepted inconsistent plan counts", err)
			}
		})
	}
}

func TestDocMarkdownPlanReviewLateCancellationAndRetry(test *testing.T) {
	source := strings.Repeat("# "+strings.Repeat("x", 96)+"\n", 500)
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	load := func(_ context.Context, from, length int) ([]byte, error) {
		return []byte(source[from : from+length]), nil
	}
	first, err := StepDocMarkdownPlan(context.Background(), state, load)
	if err != nil || first.State.OperationCount == 0 || len(first.Objects) != 0 {
		test.Fatal("fixture did not stop with a partial operation page", err)
	}
	prior, err := first.State.Encode()
	if err != nil {
		test.Fatal(err)
	}
	clean, err := StepDocMarkdownPlan(context.Background(), first.State, load)
	if err != nil || len(clean.Objects) == 0 {
		test.Fatal("next step did not stage immutable objects", err)
	}
	for _, cancelAt := range []int{1, 2} {
		test.Run(fmt.Sprint(cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			got, err := StepDocMarkdownPlan(ctx, first.State, func(ctx context.Context, from, length int) ([]byte, error) {
				calls++
				if calls == cancelAt {
					cancel()
				}
				return load(ctx, from, length)
			})
			if calls != cancelAt || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocMarkdownPlanStep{}) {
				test.Fatal("cancelled step exposed partial objects or state", calls, err)
			}
			unchanged, err := first.State.Encode()
			if err != nil || !bytes.Equal(unchanged, prior) {
				test.Fatal("cancelled step mutated caller-owned state", err)
			}
			retry, err := StepDocMarkdownPlan(context.Background(), first.State, load)
			if err != nil || !reflect.DeepEqual(retry, clean) {
				test.Fatal("retry after cancellation changed the immutable result", err)
			}
		})
	}
	compiled := markdownReviewCompile(test, source, 32)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if plan, objects, err := FinishDocMarkdownPlan(ctx, compiled.state); !errors.Is(err, context.Canceled) || plan != (DocMarkdownPlan{}) || len(objects) != 0 {
		test.Fatal("cancelled finish exposed a plan", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	page, err := ReadDocMarkdownPlanPage(ctx, compiled.plan, 0, func(_ context.Context, hash ContentHash) ([]byte, error) {
		cancel()
		return compiled.objects[hash], nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocMarkdownPlanPage{}) {
		test.Fatal("cancelled selected read exposed a page", err)
	}
}
