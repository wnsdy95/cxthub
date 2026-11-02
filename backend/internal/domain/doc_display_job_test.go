package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDocDisplayStepCommitRejectsForgedProgress(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("x", 100_000) + `"}`)
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	prior, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocDisplayStringStep){
		"valid":           func(*DocDisplayStringStep) {},
		"next":            func(step *DocDisplayStringStep) { step.State.Next++ },
		"count":           func(step *DocDisplayStringStep) { step.State.Directory.Count++ },
		"text-size":       func(step *DocDisplayStringStep) { step.State.TextBytes-- },
		"content":         func(step *DocDisplayStringStep) { step.State.HasContent = false },
		"page-start":      func(step *DocDisplayStringStep) { step.Page.From++ },
		"raw-start":       func(step *DocDisplayStringStep) { step.Page.Runs[0].SourceFrom++ },
		"missing-page":    func(step *DocDisplayStringStep) { step.Page = nil },
		"missing-object":  func(step *DocDisplayStringStep) { step.Objects = nil },
		"corrupt-object":  func(step *DocDisplayStringStep) { step.Objects[0].Bytes[0] ^= 1 },
		"read-budget":     func(step *DocDisplayStringStep) { step.ReadBytes = DocDisplayStepSourceBytes + 1 },
		"unread-progress": func(step *DocDisplayStringStep) { step.ReadBytes = 1 },
	} {
		test.Run(name, func(test *testing.T) {
			step, err := StepDocDisplayString(context.Background(), source, prior, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
			if err != nil {
				test.Fatal(err)
			}
			mutate(&step)
			err = ValidateDocDisplayStringStep(context.Background(), source, prior, step)
			if name == "valid" && err != nil || name != "valid" && !errors.Is(err, ErrIntegrity) {
				test.Fatal(err)
			}
		})
	}
}

func TestDocDisplayStepCommitAcceptsTerminalAndDirectoryCarry(test *testing.T) {
	for _, text := range []string{strings.Repeat("x", DocDisplayPageTextBytes), strings.Repeat(`\u003c`, DocDisplayPageRuns*129), "value"} {
		raw := []byte(`{"kind":"tool_result","output":"` + text + `"}`)
		source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
		if err != nil {
			test.Fatal(err)
		}
		prior, err := NewDocDisplayStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		for !prior.Done {
			step, err := StepDocDisplayString(context.Background(), source, prior, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
			if err != nil {
				test.Fatal(err)
			}
			if err := ValidateDocDisplayStringStep(context.Background(), source, prior, step); err != nil {
				test.Fatal(err)
			}
			prior = step.State
		}
	}
}
