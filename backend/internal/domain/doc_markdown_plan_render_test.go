package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocMarkdownPlanFragmentsMatchSharedTextAndCoordinates(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/markdown-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Cases []struct{ Name, Source, Text string } `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		test.Fatal("invalid fixture")
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			compiled := compileMarkdownFixture(test, scenario.Source, true)
			cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
			if err != nil {
				test.Fatal(err)
			}
			baseline, err := BuildDocMarkdownDisplay(context.Background(), scenario.Source)
			if err != nil {
				test.Fatal(err)
			}
			var text strings.Builder
			for cursor.TextBytes < compiled.plan.TextBytes {
				fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
					func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
					func(_ context.Context, from, length int) ([]byte, error) {
						return []byte(scenario.Source[from : from+length]), nil
					})
				if err != nil || fragment.From() != cursor.TextBytes || fragment.Next().TextBytes <= cursor.TextBytes {
					test.Fatal("fragment did not advance", fragment.Next(), err)
				}
				for offset, character := range fragment.Text() {
					got, err := fragment.SourceRange(offset, utf8.RuneLen(character))
					want, failure := baseline.SourceRange(fragment.From()+offset, utf8.RuneLen(character))
					if err != nil || failure != nil || got != want {
						test.Fatal("fragment changed decoded cover", got, want, err, failure)
					}
				}
				text.WriteString(fragment.Text())
				cursor = fragment.Next()
			}
			if text.String() != scenario.Text || cursor.Page != compiled.plan.Pages.Count {
				test.Fatal(text.String(), scenario.Text, cursor)
			}
		})
	}
}

func TestDocMarkdownPlanLargeFragmentsBoundSourceAndOutput(test *testing.T) {
	for _, scenario := range []struct{ name, source, want string }{
		{"large strong", "**" + strings.Repeat("한글🙂", 40000) + "**", strings.Repeat("한글🙂", 40000)},
		{"escaped encoding budget", "```\n" + strings.Repeat("<>&\x01", 70000) + "\n```", strings.Repeat("<>&\x01", 70000)},
		{"many blocks", strings.Repeat("# A **B** C\n", 20000), strings.TrimSuffix(strings.Repeat("A B C\n", 20000), "\n")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			compiled := compileMarkdownFixture(test, scenario.source, true)
			cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
			if err != nil {
				test.Fatal(err)
			}
			digest := sha256.New()
			fragments, maxText, maxRead, maxObjectLoads, maxEncoded, totalRead := 0, 0, 0, 0, 0, 0
			for cursor.TextBytes < compiled.plan.TextBytes {
				reads, objects := 0, 0
				fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
					func(_ context.Context, hash ContentHash) ([]byte, error) {
						objects++
						return compiled.objects[hash], nil
					},
					func(_ context.Context, from, length int) ([]byte, error) {
						if from < 0 || length <= 0 || length > 4096 || from > len(scenario.source)-length {
							test.Fatal("unbounded decoded read", from, length)
						}
						reads += length
						return []byte(scenario.source[from : from+length]), nil
					})
				if err != nil {
					test.Fatal("large replay failed", cursor, err)
				}
				encoded, err := canonicalJSON(fragment.page)
				if err != nil || len(encoded) > MaxDocDisplayPageBytes || len(fragment.Text()) > DocDisplayPageTextBytes || len(fragment.page.Runs) > DocDisplayPageRuns || reads > DocDisplayStepSourceBytes || fragment.ReadBytes() != reads || fragment.PlanPages() > DocMarkdownReplayPlanPages || objects > 25 || fragment.From() != cursor.TextBytes || fragment.Next().TextBytes <= cursor.TextBytes {
					test.Fatal("fragment exceeded bounds or failed to progress", len(encoded), len(fragment.Text()), reads, objects, fragment.Next(), err)
				}
				_, _ = digest.Write([]byte(fragment.Text()))
				maxText, maxRead, maxObjectLoads, maxEncoded = max(maxText, len(fragment.Text())), max(maxRead, reads), max(maxObjectLoads, objects), max(maxEncoded, len(encoded))
				totalRead += reads
				fragments++
				cursor = fragment.Next()
			}
			if actual := ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil))); actual != HashContent([]byte(scenario.want)) || cursor.TextBytes != len(scenario.want) {
				test.Fatal("streamed output changed", actual, HashContent([]byte(scenario.want)), cursor.TextBytes, len(scenario.want))
			}
			test.Logf("input_bytes=%d output_bytes=%d fragments=%d max_text=%d max_read=%d total_read=%d max_object_loads=%d max_encoded=%d", len(scenario.source), cursor.TextBytes, fragments, maxText, maxRead, totalRead, maxObjectLoads, maxEncoded)
		})
	}
}

func TestDocMarkdownPlanFragmentRejectsBadCursorBeforeIO(test *testing.T) {
	compiled := compileMarkdownFixture(test, "**alpha** beta", true)
	initial, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*DocMarkdownPlanCursor){
		func(cursor *DocMarkdownPlanCursor) { cursor.Version = 2 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Plan = HashContent([]byte("another plan")) },
		func(cursor *DocMarkdownPlanCursor) { cursor.Page = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Operation = DocMarkdownPlanOperations },
		func(cursor *DocMarkdownPlanCursor) { cursor.Byte = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.TextBytes = -1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Page = compiled.plan.Pages.Count },
	} {
		cursor := initial
		mutate(&cursor)
		called := false
		fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(context.Context, ContentHash) ([]byte, error) { called = true; return nil, ErrNotFound },
			func(context.Context, int, int) ([]byte, error) { called = true; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || called || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("invalid cursor read source", cursor, called, err)
		}
	}
	for _, mutate := range []func(*DocMarkdownPlanCursor){
		func(cursor *DocMarkdownPlanCursor) { cursor.Byte = 1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.Operation = 1 },
		func(cursor *DocMarkdownPlanCursor) { cursor.TextBytes = 1 },
	} {
		cursor := initial
		mutate(&cursor)
		textRead := false
		fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
			func(context.Context, int, int) ([]byte, error) { textRead = true; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || textRead || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("inconsistent cursor read body", cursor, textRead, err)
		}
	}
}

func TestDocMarkdownPlanFragmentCancellationAndCorruptionReturnNoOutput(test *testing.T) {
	source := "**body**"
	compiled := compileMarkdownFixture(test, source, true)
	cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelAt := range []string{"metadata", "text"} {
		ctx, cancel := context.WithCancel(context.Background())
		fragment, err := ReadDocMarkdownPlanFragment(ctx, compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) {
				if cancelAt == "metadata" {
					cancel()
				}
				return compiled.objects[hash], nil
			},
			func(_ context.Context, from, length int) ([]byte, error) {
				cancel()
				return []byte(source[from : from+length]), nil
			})
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("canceled replay exposed output", cancelAt, err)
		}
	}
	textRead := false
	fragment, err := ReadDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
		func(context.Context, ContentHash) ([]byte, error) { return []byte(`{"corrupt":true}`), nil },
		func(context.Context, int, int) ([]byte, error) { textRead = true; return nil, ErrNotFound })
	if !errors.Is(err, ErrIntegrity) || textRead || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
		test.Fatal("corrupt metadata read body", textRead, err)
	}
}
