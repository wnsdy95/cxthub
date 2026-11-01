package app

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func preparedFindReviewSource(test *testing.T, text string) docViewSource {
	test.Helper()
	raw, err := json.Marshal(map[string]any{"kind": "tool_result", "output": text})
	if err != nil || len(raw) <= docViewBytes {
		test.Fatal("review fixture must use oversized path", err)
	}
	ctx := context.Background()
	plan, err := domain.BuildDocDisplayStringSourceReader(ctx, domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	state, err := domain.NewDocDisplayStringState(plan)
	if err != nil {
		test.Fatal(err)
	}
	objects := map[domain.ContentHash][]byte{}
	for !state.Done {
		step, err := domain.StepDocDisplayString(ctx, plan, state, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.Objects {
			objects[object.Hash] = object.Bytes
		}
		state = step.State
	}
	generation, tail, err := domain.FinishDocDisplayString(ctx, plan, state)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range tail {
		objects[object.Hash] = object.Bytes
	}
	entry := domain.DocRangeEntry{DocEventIndex: domain.DocEventIndex{Hash: plan.EventHash, Length: len(raw)}, Kind: domain.EventToolResult}
	return docViewSource{total: 1,
		entry: func(int) (domain.DocRangeEntry, error) { return entry, nil },
		read: func(int, int) ([]byte, error) {
			test.Error("prepared scan read the original source")
			return nil, domain.ErrIntegrity
		},
		displayGeneration: func(domain.DocEventIndex) (domain.DocDisplayStringGeneration, error) { return generation, nil },
		displayPage: func(_ domain.DocEventIndex, page int) (domain.DocDisplayPage, error) {
			return domain.ReadDocDisplayPage(ctx, generation, page, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				return objects[hash], nil
			})
		},
	}
}

func TestDocPreparedScanMatchesWholeTextAcrossResultAndPageLimits(test *testing.T) {
	for _, scenario := range []struct{ name, text, query string }{
		{"many-overlaps", strings.Repeat("z", domain.DocDisplayPageTextBytes-8) + strings.Repeat("a", 100) + strings.Repeat("z", 150_000), "aaa"},
		{"unicode-max-query", strings.Repeat("z", domain.DocDisplayPageTextBytes-8) + strings.Repeat("界", 270) + strings.Repeat("z", 150_000), strings.Repeat("界", domain.MaxDocFindQueryRunes)},
		{"unicode-fold-boundary", strings.Repeat("z", domain.DocDisplayPageTextBytes-1) + "KKk" + strings.Repeat("z", 150_000), "k"},
		{"escape-runs", strings.Repeat("x\n", 510) + strings.Repeat("a", 100) + strings.Repeat("z", 150_000), "aaa"},
		{"omitted-whitespace", strings.Repeat(" \t\n", 80_000), "x"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			source := preparedFindReviewSource(test, scenario.text)
			selector := docFindProgress{To: 1, Representation: domain.DocFindPreparedRepresentation}
			var got, want []int
			for calls := 0; selector.Index < selector.To && calls < 100; calls++ {
				before := selector
				page, err := scanDocFindRange(context.Background(), source, &selector, scenario.query)
				if err != nil || page.Prepared == nil || len(page.Matches) > domain.DocFindPageMatches {
					test.Fatal(page, err)
				}
				if selector.Index == before.Index && (selector.Prepared.Page < before.Prepared.Page || selector.Prepared.Page == before.Prepared.Page && selector.Byte <= before.Byte) {
					test.Fatal("prepared scan made no progress", before, selector)
				}
				for _, match := range page.Matches {
					if match.SourceRange == nil || match.PreparedPage == nil || match.Raw || match.Field != 0 {
						test.Fatal("lost prepared coordinates", match)
					}
					got = append(got, match.Offset)
				}
			}
			if selector.Index != selector.To {
				test.Fatal("did not reach terminal event", selector)
			}
			for position := 0; position < len(scenario.text); {
				matches, err := domain.FindDocText(context.Background(), scenario.text, scenario.query, position, domain.DocFindPageMatches)
				if err != nil {
					test.Fatal(err)
				}
				if len(matches) == 0 {
					break
				}
				for _, match := range matches {
					want = append(want, match.Offset)
					_, width := utf8.DecodeRuneInString(scenario.text[match.Offset:])
					position = match.Offset + width
				}
			}
			if !reflect.DeepEqual(got, want) {
				test.Fatal("prepared paging lost/duplicated/reordered matches", got, want)
			}
		})
	}
}
