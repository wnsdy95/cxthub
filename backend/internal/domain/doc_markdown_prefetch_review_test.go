package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocMarkdownPrefetchTracksRemainingOutput(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		source string
		budget int
		want   string
		reads  int
	}{
		{name: "tiny-ascii", source: strings.Repeat("a", 8192), budget: 4, want: "aaaa", reads: 1},
		{name: "short-ascii", source: strings.Repeat("a", 8192), budget: 37, want: strings.Repeat("a", 37), reads: 1},
		{name: "rune-exact-boundary", source: "🙂abcdef", budget: 4, want: "🙂", reads: 1},
		{name: "rune-after-boundary", source: "aaaa🙂abcdef", budget: 4, want: "aaaa", reads: 1},
		{name: "rune-needs-refill", source: "a🙂abcdef", budget: 4, want: "a", reads: 2},
		{name: "default-output-unchanged", source: strings.Repeat("a", 8192), budget: DocDisplayPageTextBytes, want: strings.Repeat("a", 8192), reads: 2},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			compiled := compileMarkdownFixture(test, fixture.source, true)
			cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
			if err != nil {
				test.Fatal(err)
			}
			var reads [][2]int
			fragment, err := readDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
				func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
				func(_ context.Context, from, length int) ([]byte, error) {
					reads = append(reads, [2]int{from, length})
					return []byte(fixture.source[from : from+length]), nil
				}, docMarkdownReplayPolicy{MaxBytes: fixture.budget})
			if err != nil || fragment.Text() != fixture.want || fragment.Next().TextBytes != len(fixture.want) || len(reads) != fixture.reads {
				test.Fatal("prefetch changed text, cursor, or required UTF8 reads", len(fragment.Text()), len(fixture.want), fragment.Next(), reads, err)
			}
			physical := 0
			for _, read := range reads {
				if read[1] > min(4096, max(utf8.UTFMax, fixture.budget)) {
					test.Fatal("tiny output prefetched an unrelated decoded-text suffix", read, fixture.budget)
				}
				physical += read[1]
			}
			if fragment.ReadBytes() != physical {
				test.Fatal("prefetch bytes were not charged", fragment.ReadBytes(), physical)
			}
			baseline, err := BuildDocMarkdownDisplay(context.Background(), fixture.source)
			if err != nil {
				test.Fatal(err)
			}
			got, err := fragment.SourceRange(0, len(fixture.want))
			want, baselineErr := baseline.SourceRange(0, len(fixture.want))
			if err != nil || baselineErr != nil || got != want {
				test.Fatal("bounded prefetch changed source coordinates", got, want, err, baselineErr)
			}
		})
	}
}

func TestDocMarkdownPrefetchCurrentInputFailuresRemainAtomic(test *testing.T) {
	compiled := compileMarkdownFixture(test, strings.Repeat("a", 8192), true)
	cursor, err := NewDocMarkdownPlanCursor(compiled.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, failure := range []error{ErrIntegrity, ErrNotFound, context.Canceled} {
		fragment, err := readDocMarkdownPlanFragment(context.Background(), compiled.plan, cursor,
			func(_ context.Context, hash ContentHash) ([]byte, error) { return compiled.objects[hash], nil },
			func(context.Context, int, int) ([]byte, error) { return nil, failure },
			docMarkdownReplayPolicy{MaxBytes: 4})
		if !errors.Is(err, failure) || !reflect.DeepEqual(fragment, DocMarkdownPlanFragment{}) {
			test.Fatal("current source failure exposed partial output", err, failure)
		}
	}
}
