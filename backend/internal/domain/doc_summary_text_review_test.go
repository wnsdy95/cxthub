package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func summaryReviewRaw(raw []byte) func(context.Context, int, int) ([]byte, error) {
	return func(_ context.Context, from, length int) ([]byte, error) {
		if from < 0 || length <= 0 || length > 4096 || length > len(raw) || from > len(raw)-length {
			return nil, ErrIntegrity
		}
		return raw[from : from+length], nil
	}
}

func summaryReviewFirstStep(test *testing.T, fixture summaryTextFixture, raw []byte) DocSummaryTextStep {
	test.Helper()
	initial, err := NewDocSummaryTextState(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocSummaryText(context.Background(), fixture.source, initial, summaryReviewRaw(raw), summaryFixtureLoad(fixture))
	if err != nil || step.State.Done {
		test.Fatal("review needs an unfinished first step", err)
	}
	return step
}

func TestDocSummaryTextReviewRejectsActiveNextReset(test *testing.T) {
	for _, block := range []int{0, 1} {
		test.Run(fmt.Sprint(block), func(test *testing.T) {
			prefix := ""
			if block > 0 {
				prefix = `{"text":"head"},`
			}
			raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[` + prefix + `{"text":"` + strings.Repeat("abcdefghijklmnop", 7000) + `"}]}`)
			fixture := buildSummaryTextFixture(test, raw)
			first := summaryReviewFirstStep(test, fixture, raw)
			if first.State.Block != block || first.State.Next == 0 || !first.State.Joined || first.State.Directory.Count == 0 {
				test.Fatal("review needs decoded text in the active block", first.State.Block, first.State.Next)
			}
			forged := first.State
			forged.Next = 0
			proofErr := ValidateDocSummaryTextCheckpoint(context.Background(), fixture.source, forged, summaryFixtureLoad(fixture))
			step, err := StepDocSummaryText(context.Background(), fixture.source, forged, summaryReviewRaw(raw), summaryFixtureLoad(fixture))
			if !errors.Is(proofErr, ErrIntegrity) || !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocSummaryTextStep{}) {
				test.Fatalf("active raw cursor reset replayed text: block=%d original_next=%d bytes=%d/%d next=%d proof=%v step=%v", block, first.State.Next, first.State.TextBytes, step.State.TextBytes, step.State.Next, proofErr, err)
			}
		})
	}
}

func TestDocSummaryTextReviewRejectsEmptyCheckpointSkippingText(test *testing.T) {
	for name, body := range map[string]string{
		"skip-first-block": `{"text":"must not disappear"},{"text":"tail"}`,
		"false-done":       `{"text":"must not disappear"}`,
	} {
		test.Run(name, func(test *testing.T) {
			raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[` + body + `]}`)
			fixture := buildSummaryTextFixture(test, raw)
			forged, err := NewDocSummaryTextState(fixture.source)
			if err != nil {
				test.Fatal(err)
			}
			forged.Block = 1
			if name == "false-done" {
				forged.Done, forged.Hash = true, HashContent(nil)
				forged.HashState = nil
			}
			proofErr := ValidateDocSummaryTextCheckpoint(context.Background(), fixture.source, forged, summaryFixtureLoad(fixture))
			if forged.Done {
				generation, objects, err := FinishDocSummaryText(context.Background(), fixture.source, forged, summaryFixtureLoad(fixture))
				if !errors.Is(proofErr, ErrIntegrity) || !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocSummaryTextGeneration{}) || objects != nil {
					test.Fatalf("false empty completion published: bytes=%d pages=%d hash=%s proof=%v finish=%v", generation.TextBytes, generation.Pages.Count, generation.Hash, proofErr, err)
				}
				return
			}
			step, err := StepDocSummaryText(context.Background(), fixture.source, forged, summaryReviewRaw(raw), summaryFixtureLoad(fixture))
			if !errors.Is(proofErr, ErrIntegrity) || !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocSummaryTextStep{}) {
				test.Fatalf("empty checkpoint skipped nonempty first block: bytes=%d done=%t hash=%s proof=%v step=%v", step.State.TextBytes, step.State.Done, step.State.Hash, proofErr, err)
			}
		})
	}
}

func TestDocSummaryTextReviewCheckpointCancellationWinsLoadFailure(test *testing.T) {
	raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[{"text":"body"},{"text":"tail"}]}`)
	fixture := buildSummaryTextFixture(test, raw)
	failure := errors.New("checkpoint object unavailable")
	loads := 0
	if err := ValidateDocSummaryTextCheckpoint(context.Background(), fixture.source, fixture.state, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		loads++
		return summaryFixtureLoad(fixture)(ctx, hash)
	}); err != nil {
		test.Fatal(err)
	}
	for failAt := 1; failAt <= loads; failAt++ {
		test.Run(fmt.Sprint(failAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := ValidateDocSummaryTextCheckpoint(ctx, fixture.source, fixture.state, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				calls++
				if calls == failAt {
					cancel()
					return nil, failure
				}
				return summaryFixtureLoad(fixture)(ctx, hash)
			})
			if !errors.Is(err, context.Canceled) {
				test.Fatal("cancelled checkpoint exposed load failure instead of cancellation", err)
			}
		})
	}
}

func TestDocSummaryTextReviewPendingAndLastPageLinks(test *testing.T) {
	for name, body := range map[string]string{
		"pending": strings.Repeat(`{},`, 129) + `{}`,
		"sealed":  `{"text":"` + strings.Repeat(`\n`, 1700) + `"}`,
	} {
		test.Run(name, func(test *testing.T) {
			raw := []byte(`{"kind":"message","compact_summary":true,"blocks":[` + body + `]}`)
			fixture := buildSummaryTextFixture(test, raw)
			first := summaryReviewFirstStep(test, fixture, raw)
			if name == "pending" && first.State.Pending == "" || name == "sealed" && first.State.Directory.Count == 0 {
				test.Fatal("checkpoint fixture has the wrong page form")
			}
			before, err := first.State.Encode(fixture.source)
			if err != nil {
				test.Fatal(err)
			}
			for mutation, mutate := range map[string]func(*DocSummaryTextState){
				"block-rewind": func(state *DocSummaryTextState) { state.Block-- },
				"block-skip":   func(state *DocSummaryTextState) { state.Block++ },
				"joined":       func(state *DocSummaryTextState) { state.Joined = !state.Joined },
				"next":         func(state *DocSummaryTextState) { state.Next++ },
				"text-end":     func(state *DocSummaryTextState) { state.TextBytes++ },
				"page-end":     func(state *DocSummaryTextState) { state.PageFrom++ },
				"pending":      func(state *DocSummaryTextState) { state.Pending = HashContent([]byte("wrong pending page")) },
			} {
				test.Run(mutation, func(test *testing.T) {
					forged := first.State
					mutate(&forged)
					step, err := StepDocSummaryText(context.Background(), fixture.source, forged, summaryReviewRaw(raw), summaryFixtureLoad(fixture))
					if err == nil || !reflect.DeepEqual(step, DocSummaryTextStep{}) {
						test.Fatal("broken checkpoint link accepted", step.State, err)
					}
					after, err := first.State.Encode(fixture.source)
					if err != nil || !bytes.Equal(before, after) {
						test.Fatal("rejected checkpoint mutated its original", err)
					}
				})
			}
		})
	}
}
