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
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func markdownReviewReplay(test *testing.T, source string, compiled markdownReviewCompilation) (string, DocDisplayPage) {
	test.Helper()
	var text strings.Builder
	var runs []DocDisplayRun
	operations, sourceEnd := 0, 0
	for index := 0; index < compiled.plan.Pages.Count; index++ {
		page, err := ReadDocMarkdownPlanPage(context.Background(), compiled.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			return compiled.objects[hash], nil
		})
		if err != nil || page.From != text.Len() {
			test.Fatal("invalid page sequence", index, err)
		}
		for _, operation := range page.Operations {
			if operation.From < sourceEnd || operation.From >= operation.To || operation.To > len(source) || !utf8.RuneStart(source[operation.From]) || operation.To < len(source) && !utf8.RuneStart(source[operation.To]) {
				test.Fatal("invalid decoded source cover", operation, sourceEnd)
			}
			if operation.Text == "" {
				text.WriteString(source[operation.From:operation.To])
			} else {
				text.WriteString(operation.Text)
			}
			runs = append(runs, DocDisplayRun{To: text.Len(), SourceFrom: operation.From, SourceTo: operation.To, Linear: operation.TextBytes() == operation.To-operation.From})
			sourceEnd = operation.To
			operations++
		}
	}
	if operations != compiled.plan.OperationCount || text.Len() != compiled.plan.TextBytes {
		test.Fatal("plan counts disagree with replay", operations, text.Len(), compiled.plan)
	}
	return text.String(), DocDisplayPage{Version: 2, Text: text.String(), Runs: runs}
}

func TestDocMarkdownPlanReviewPathologicalDelimiterPasses(test *testing.T) {
	for _, scenario := range []struct{ name, source, want string }{
		{"unmatched brackets", "prefix " + strings.Repeat("[", 150000), "prefix " + strings.Repeat("[", 150000)},
		{"rejected strong delimiters", "prefix " + strings.Repeat("*", 150000), "prefix " + strings.Repeat("*", 150000)},
		{"empty code delimiters", "prefix " + strings.Repeat("`", 150000), "prefix " + strings.Repeat("`", 150000)},
		{"shared unterminated URL", "prefix " + strings.Repeat("[", 70000) + "](https://" + strings.Repeat("x", 90000), "prefix " + strings.Repeat("[", 70000) + "](https://" + strings.Repeat("x", 90000)},
		{"nonrecursive large label", "[" + strings.Repeat("[`**🙂", 20000) + "](https://hidden.test)", strings.Repeat("[`**🙂", 20000)},
		{"trailing paragraph newline", strings.Repeat("line 🙂\n", 20000), strings.TrimSuffix(strings.Repeat("line 🙂\n", 20000), "\n")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if len(scenario.source) <= MaxDocMarkdownDisplayBytes {
				test.Fatal("fixture does not exercise the actual large compiler")
			}
			compiled := markdownReviewCompile(test, scenario.source, 96)
			got, _ := markdownReviewReplay(test, scenario.source, compiled)
			if got != scenario.want || compiled.readBytes > 6*len(scenario.source)+DocDisplayStepSourceBytes {
				test.Fatal("large pathological input changed text or rescanned unbounded suffixes", len(got), len(scenario.want), compiled.readBytes)
			}
			test.Logf("input=%d output=%d steps=%d read=%d", len(scenario.source), len(got), compiled.steps, compiled.readBytes)
		})
	}
}

func TestDocMarkdownPlanReviewDeterministicGrammarParity(test *testing.T) {
	fixtures := []string{"first\n\nsecond", "# \r\u2028\u2029", "- ** a **\t` b `", "# a\n```\n```\n# b", "[a[b](https://x)", "**a***b**", "before\n\n\n\n", "\u0085\n\ufeff\nend"}
	fragments := []string{"a", "\ud55c\uae00🙂", "# ", "###\u00a0", "#### ", "- ", "* ", "01) ", "1.\t", "```", "``", "**", "*", "`", "[", "](https://", "](http://", "](HTTPS://", "](javascript:", "x)", " ", "\t", "\r", "\n", "\n\n", "\u2028", "\u2029", "\ufeff", "\u0085", "\v\f"}
	random := rand.New(rand.NewSource(20261021))
	for count := 0; count < 600; count++ {
		var source strings.Builder
		for fragment := 0; fragment < 1+random.Intn(40); fragment++ {
			source.WriteString(fragments[random.Intn(len(fragments))])
		}
		fixtures = append(fixtures, source.String())
	}
	for index, source := range fixtures {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			baseline, err := BuildDocMarkdownDisplay(context.Background(), source)
			if err != nil {
				test.Fatal(err)
			}
			compiled := markdownReviewCompile(test, source, 32)
			got, mapped := markdownReviewReplay(test, source, compiled)
			if got != baseline.Text() || compiled.plan.LeadingBreaks != baseline.leadingBreaks || compiled.plan.TrailingBreaks != baseline.trailingBreaks {
				test.Fatalf("source=%q got=%q want=%q breaks=%d/%d want=%d/%d", source, got, baseline.Text(), compiled.plan.LeadingBreaks, compiled.plan.TrailingBreaks, baseline.leadingBreaks, baseline.trailingBreaks)
			}
			for offset, character := range got {
				span, err := mapped.SourceRange(offset, utf8.RuneLen(character))
				want, failure := baseline.SourceRange(offset, utf8.RuneLen(character))
				if err != nil || failure != nil || span != want {
					test.Fatalf("source=%q output=%q offset=%d cover=%+v want=%+v errors=%v/%v", source, got, offset, span, want, err, failure)
				}
			}
		})
	}
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

func TestDocMarkdownPlanReviewRejectFutureDelimiterCache(test *testing.T) {
	source := strings.Repeat("**x** ", 80) + "\n# tail"
	state, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte(source)), Bytes: len(source)})
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocMarkdownPlan(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
		return []byte(source[from : from+length]), nil
	})
	if err != nil || step.State.Phase != docMarkdownPlanInline || step.State.Block.To >= len(source) {
		test.Fatal("fixture did not yield inside a block preceding another block", step.State.Phase, err)
	}
	step.State.NextDelimiter[0] = len(source)
	if !errors.Is(step.State.Validate(), ErrIntegrity) {
		continued, failure := StepDocMarkdownPlan(context.Background(), step.State, func(_ context.Context, from, length int) ([]byte, error) {
			return []byte(source[from : from+length]), nil
		})
		baseline, err := BuildDocMarkdownDisplay(context.Background(), source)
		if err != nil {
			test.Fatal(err)
		}
		test.Fatalf("accepted future cached star outside block %d..%d: cache=%d cursor=%d; continuation bytes=%d want=%d phase=%d error=%v", step.State.Block.From, step.State.Block.To, step.State.NextDelimiter[0], step.State.InlineCursor, continued.State.TextBytes, len(baseline.Text()), continued.State.Phase, failure)
	}
	raw, err := canonicalJSON(step.State)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeDocMarkdownPlanState(HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("rehashed future delimiter cache accepted", err)
	}
}

func TestDocMarkdownPlanReviewFragmentEqualWidthSyntheticCover(test *testing.T) {
	source := "a\n\nb"
	compiled := markdownReviewCompile(test, source, 8)
	cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
		func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
		func(_ context.Context, from, length int) ([]byte, error) {
			return []byte(source[from : from+length]), nil
		})
	if err != nil || fragment.Text() != source {
		test.Fatal("fragment changed paragraph text", fragment.Text(), err)
	}
	baseline, err := BuildDocMarkdownDisplay(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	for offset := range source {
		got, err := fragment.SourceRange(offset, 1)
		want, failure := baseline.SourceRange(offset, 1)
		if err != nil || failure != nil || got != want {
			test.Fatalf("source=%q offset=%d decoded cover=%+v want=%+v errors=%v/%v", source, offset, got, want, err, failure)
		}
	}
}
