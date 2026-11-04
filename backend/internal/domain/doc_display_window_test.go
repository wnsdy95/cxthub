package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func displayWindowFixture(test *testing.T, text string) (DocDisplayStringSource, []DocDisplayPage, []byte) {
	test.Helper()
	raw, err := json.Marshal(map[string]any{"kind": "tool_result", "output": text})
	if err != nil {
		test.Fatal(err)
	}
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), strings.NewReader(string(raw)))
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	var pages []DocDisplayPage
	for !state.Done {
		step, err := StepDocDisplayString(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil })
		if err != nil {
			test.Fatal(err)
		}
		if step.Page != nil {
			pages = append(pages, *step.Page)
		}
		state = step.State
	}
	return source, pages, raw
}

func TestDocDisplaySearchWindowFindsCrossPageUnicodeAndLongestQueries(test *testing.T) {
	for _, query := range []string{"Needle🙂", strings.Repeat("a", 255) + "b"} {
		prefix := strings.Repeat("x", DocDisplayPageTextBytes-3)
		if len(query) == 256 {
			prefix = strings.Repeat("x", DocDisplayPageTextBytes-255)
		}
		source, pages, raw := displayWindowFixture(test, prefix+query+strings.Repeat("z", 1000))
		if len(pages) != 2 {
			test.Fatal("fixture did not split", len(pages))
		}
		var matches []DocFindMatch
		for index, current := range pages {
			var previous *DocDisplayPage
			if index > 0 {
				previous = &pages[index-1]
			}
			window, err := BuildDocDisplaySearchWindow(source, previous, current)
			if err != nil {
				test.Fatal(err)
			}
			if len(window.Text()) > DocDisplayPageTextBytes+4*(MaxDocFindQueryRunes-1) {
				test.Fatal("window grew beyond selected page and prefix")
			}
			found, next, err := FindDocDisplayWindow(context.Background(), window, strings.ToLower(query), 0, 20)
			if err != nil || next != window.To() {
				test.Fatal(found, next, err)
			}
			matches = append(matches, found...)
		}
		if len(matches) != 1 || matches[0].Offset != len(prefix) || matches[0].SourceRange == nil || matches[0].Preview == "" {
			test.Fatal(matches)
		}
		span := matches[0].SourceRange
		if string(raw[span.From:span.To]) != query {
			test.Fatal("wrong crosspage source cover", string(raw[span.From:span.To]), query)
		}
	}
}

func TestDocDisplaySearchWindowPreservesOverlapsAcrossResultAndTextPages(test *testing.T) {
	prefix := strings.Repeat("x", DocDisplayPageTextBytes-3)
	source, pages, raw := displayWindowFixture(test, prefix+strings.Repeat("a", 200)+"!")
	var matches []DocFindMatch
	for index, current := range pages {
		var previous *DocDisplayPage
		if index > 0 {
			previous = &pages[index-1]
		}
		window, err := BuildDocDisplaySearchWindow(source, previous, current)
		if err != nil {
			test.Fatal(err)
		}
		position := 0
		for iteration := 0; iteration < 20; iteration++ {
			found, next, err := FindDocDisplayWindow(context.Background(), window, "aaa", position, 20)
			if err != nil {
				test.Fatal(err)
			}
			matches = append(matches, found...)
			if next == window.To() {
				break
			}
			if next <= position {
				test.Fatal("result cursor stalled")
			}
			position = next
		}
	}
	if len(matches) != 198 {
		test.Fatal("overlapping matches lost or duplicated", len(matches))
	}
	for index, match := range matches {
		if match.Offset != len(prefix)+index || match.SourceRange == nil {
			test.Fatal(index, match)
		}
		span := match.SourceRange
		if string(raw[span.From:span.To]) != "aaa" {
			test.Fatal(index, match)
		}
	}
}

func TestDocDisplaySearchWindowRejectsMixedPagesAndKeepsImmutableRuns(test *testing.T) {
	source, pages, _ := displayWindowFixture(test, strings.Repeat("a", DocDisplayPageTextBytes)+"needle")
	window, err := BuildDocDisplaySearchWindow(source, &pages[0], pages[1])
	if err != nil {
		test.Fatal(err)
	}
	pages[1].Runs[0].SourceFrom = 0
	found, _, err := FindDocDisplayWindow(context.Background(), window, "needle", 0, 20)
	if err != nil || len(found) != 1 || found[0].SourceRange.From == 0 {
		test.Fatal("window retained mutable caller provenance", found, err)
	}
	if _, err := BuildDocDisplaySearchWindow(source, &pages[0], pages[1]); !errors.Is(err, ErrIntegrity) {
		test.Fatal("mutated page accepted", err)
	}
	if _, err := BuildDocDisplaySearchWindow(source, nil, pages[1]); !errors.Is(err, ErrIntegrity) {
		test.Fatal("missing previous context accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, _, err := FindDocDisplayWindow(ctx, window, "needle", 0, 20); !errors.Is(err, context.Canceled) || len(found) != 0 {
		test.Fatal(found, err)
	}
}
