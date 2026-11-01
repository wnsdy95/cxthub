package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func markdownFixtureOperations(test testing.TB, fixture markdownPlanFixture) []DocMarkdownPlanOperation {
	test.Helper()
	var operations []DocMarkdownPlanOperation
	from := 0
	for index := 0; index < fixture.plan.Pages.Count; index++ {
		page, err := ReadDocMarkdownPlanPage(context.Background(), fixture.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			raw, found := fixture.objects[hash]
			if !found {
				return nil, ErrNotFound
			}
			return raw, nil
		})
		if err != nil || page.From != from {
			test.Fatal("plan page sequence invalid", index, page.From, from, err)
		}
		for _, operation := range page.Operations {
			from += operation.TextBytes()
		}
		operations = append(operations, page.Operations...)
	}
	if len(operations) != fixture.plan.OperationCount || from != fixture.plan.TextBytes {
		test.Fatal("incomplete operations", len(operations), from)
	}
	return operations
}

func replayMarkdownFixture(test testing.TB, source string, fixture markdownPlanFixture) (string, DocDisplayPage) {
	test.Helper()
	var text strings.Builder
	var runs []DocDisplayRun
	for _, operation := range markdownFixtureOperations(test, fixture) {
		if operation.Text != "" {
			text.WriteString(operation.Text)
		} else {
			text.WriteString(source[operation.From:operation.To])
		}
		runs = append(runs, DocDisplayRun{To: text.Len(), SourceFrom: operation.From, SourceTo: operation.To, Linear: operation.linearCoordinates()})
	}
	return text.String(), DocDisplayPage{Version: 2, Text: text.String(), Runs: runs}
}

func TestDocMarkdownPlanSharedGoldens(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/markdown-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Cases []struct{ Name, Source, Text string } `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Cases) < 40 {
		test.Fatal("invalid Markdown goldens")
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			compiled := compileMarkdownFixture(test, scenario.Source, true)
			text, mapped := replayMarkdownFixture(test, scenario.Source, compiled)
			if text != scenario.Text {
				test.Fatalf("got %q, want %q", text, scenario.Text)
			}
			baseline, err := BuildDocMarkdownDisplay(context.Background(), scenario.Source)
			if err != nil || compiled.plan.LeadingBreaks != baseline.leadingBreaks || compiled.plan.TrailingBreaks != baseline.trailingBreaks {
				test.Fatal("block separators changed", compiled.plan, baseline.leadingBreaks, baseline.trailingBreaks, err)
			}
			for offset, character := range text {
				span, err := mapped.SourceRange(offset, utf8.RuneLen(character))
				want, failure := baseline.SourceRange(offset, utf8.RuneLen(character))
				if err != nil || failure != nil || span != want {
					test.Fatal("decoded source cover changed", offset, span, want, err, failure)
				}
			}
		})
	}
}

func TestDocMarkdownPlanLargeRealInputAndRestart(test *testing.T) {
	for _, scenario := range []struct{ name, source, want string }{
		{"paragraph", strings.Repeat("long 🙂 paragraph\n", 14000), strings.TrimSuffix(strings.Repeat("long 🙂 paragraph\n", 14000), "\n")},
		{"strong", "**" + strings.Repeat("한글🙂", 40000) + "**", strings.Repeat("한글🙂", 40000)},
		{"hidden URL", "[visible](https://example.test/" + strings.Repeat("x", 300000) + ")", "visible"},
		{"unclosed code", "`" + strings.Repeat("x", 300000), "`" + strings.Repeat("x", 300000)},
		{"fence", "```\n" + strings.Repeat("raw **🙂**\n", 20000) + "```", strings.TrimSuffix(strings.Repeat("raw **🙂**\n", 20000), "\n")},
		{"long whitespace", "# " + strings.Repeat(" ", 300000) + "done", "done"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if len(scenario.source) <= MaxDocMarkdownDisplayBytes {
				test.Fatal("not a large input")
			}
			plain := compileMarkdownFixture(test, scenario.source, false)
			resumed := compileMarkdownFixture(test, scenario.source, true)
			if plain.plan != resumed.plan || !reflect.DeepEqual(plain.objects, resumed.objects) {
				test.Fatal("resumption changed immutable plan")
			}
			text, _ := replayMarkdownFixture(test, scenario.source, resumed)
			if text != scenario.want || len(text) != resumed.plan.TextBytes || resumed.steps < 2 {
				test.Fatal("large compiler lost text", len(text), len(scenario.want), resumed.steps)
			}
			if resumed.readBytes > 12*len(scenario.source)+DocDisplayStepSourceBytes {
				test.Fatal("source rereads grew beyond bounded passes", resumed.readBytes, len(scenario.source))
			}
			test.Logf("input_bytes=%d text_bytes=%d steps=%d source_read_bytes=%d checkpoint_bytes=%d operations=%d", len(scenario.source), len(text), resumed.steps, resumed.readBytes, resumed.maxCheckpoint, resumed.plan.OperationCount)
		})
	}
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

func FuzzDocMarkdownPlanParity(fuzz *testing.F) {
	for _, source := range []string{"", "# A ** B**", "[label](https://hidden)", "a\n```\n```\nb", "`x` **y**", "# h\r", "- \u2028body"} {
		fuzz.Add(source)
	}
	fuzz.Fuzz(func(test *testing.T, source string) {
		if len(source) > 2048 || !utf8.ValidString(source) {
			test.Skip()
		}
		baseline, err := BuildDocMarkdownDisplay(context.Background(), source)
		if err != nil {
			test.Fatal(err)
		}
		compiled := compileMarkdownFixture(test, source, true)
		got, _ := replayMarkdownFixture(test, source, compiled)
		if got != baseline.Text() || compiled.plan.LeadingBreaks != baseline.leadingBreaks || compiled.plan.TrailingBreaks != baseline.trailingBreaks {
			test.Fatalf("input=%q got=%q want=%q separators=%d/%d want=%d/%d", source, got, baseline.Text(), compiled.plan.LeadingBreaks, compiled.plan.TrailingBreaks, baseline.leadingBreaks, baseline.trailingBreaks)
		}
		cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
		if err != nil {
			test.Fatal(err)
		}
		for cursor.TextBytes < compiled.plan.TextBytes {
			fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
				func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
				func(_ context.Context, from, length int) ([]byte, error) {
					return []byte(source[from : from+length]), nil
				})
			if err != nil || fragment.Next().TextBytes <= cursor.TextBytes {
				test.Fatal("fuzz fragment did not progress", err)
			}
			checked := 0
			for offset, character := range fragment.Text() {
				actual, err := fragment.SourceRange(offset, utf8.RuneLen(character))
				expected, failure := baseline.SourceRange(fragment.From()+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || actual != expected {
					test.Fatal("fuzz decoded cover changed", source, offset, actual, expected, err, failure)
				}
				checked++
				if checked == 128 {
					break
				}
			}
			cursor = fragment.Next()
		}
	})
}
