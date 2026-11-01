package domain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docDisplayReviewSource(test *testing.T, output string) (domain.DocDisplayStringSource, []byte) {
	test.Helper()
	token, err := json.Marshal(output)
	if err != nil {
		test.Fatal(err)
	}
	raw := append(append([]byte(`{"kind":"tool_result","output":`), token...), '}')
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	return source, raw
}

func docDisplayReviewInitial(test *testing.T, source domain.DocDisplayStringSource) domain.DocDisplayStringState {
	test.Helper()
	state, err := domain.NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	return state
}

func docDisplayReviewStep(test *testing.T, source domain.DocDisplayStringSource, prior domain.DocDisplayStringState, raw []byte) domain.DocDisplayStringStep {
	test.Helper()
	step, err := domain.StepDocDisplayString(context.Background(), source, prior, func(_ context.Context, offset, length int) ([]byte, error) {
		return raw[offset : offset+length], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if err := domain.ValidateDocDisplayStringStep(context.Background(), source, prior, step); err != nil {
		test.Fatal("genuine step rejected", err)
	}
	return step
}

func docDisplayReviewReadFinished(source domain.DocDisplayStringSource, state domain.DocDisplayStringState, objects []domain.DocRangeObject) (domain.DocDisplayPage, error) {
	ctx := context.Background()
	generation, finished, err := domain.FinishDocDisplayString(ctx, source, state)
	if err != nil {
		return domain.DocDisplayPage{}, err
	}
	bodies := make(map[domain.ContentHash][]byte)
	for _, object := range append(objects, finished...) {
		bodies[object.Hash] = object.Bytes
	}
	return domain.ReadDocDisplayPage(ctx, generation, generation.Pages.Count-1, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		raw, found := bodies[hash]
		if !found {
			return nil, domain.ErrNotFound
		}
		return raw, nil
	})
}

func TestDocDisplayReviewRejectsImpossibleRunForgery(test *testing.T) {
	for _, scenario := range []struct {
		name, output, forged string
		linear               bool
	}{
		{"twelve-byte-escape-to-ascii", "abcdefghijkl", "X", false},
		{"six-byte-escape-to-non-bmp", "abcdef", "🙂", false},
		{"invalid-simple-escape", "ab", "X", false},
		{"literal-quote", "ab", "\"b", true},
		{"literal-backslash", "ab", "\\b", true},
		{"literal-control", "ab", "\x00b", true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			source, _ := docDisplayReviewSource(test, scenario.output)
			prior := docDisplayReviewInitial(test, source)
			page := domain.DocDisplayPage{Version: 1, Source: prior.Source, Text: scenario.forged,
				Runs: []domain.DocDisplayRun{{To: len(scenario.forged), SourceFrom: source.Token.From + 1, SourceTo: source.Token.To - 1, Linear: scenario.linear}}}
			encoded, err := page.Encode(source)
			if errors.Is(err, domain.ErrIntegrity) {
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			next := prior
			next.Next, next.Done, next.TextBytes, next.HasContent = source.Token.To, true, len(page.Text), true
			next.Directory, _, err = prior.Directory.Append(context.Background(), domain.HashContent(encoded))
			if err != nil {
				test.Fatal(err)
			}
			objects := []domain.DocRangeObject{{Hash: domain.HashContent(encoded), Bytes: encoded}}
			step := domain.DocDisplayStringStep{State: next, Page: &page, Objects: objects, ReadBytes: source.Token.To - source.Token.From}
			if err := domain.ValidateDocDisplayStringStep(context.Background(), source, prior, step); !errors.Is(err, domain.ErrIntegrity) {
				visible, readErr := docDisplayReviewReadFinished(source, next, objects)
				test.Errorf("impossible decoding accepted: original=%q forged=%q validation=%v published=%q read=%v", scenario.output, scenario.forged, err, visible.Text, readErr)
			}
		})
	}
}

func docDisplayReviewCheckpoint(test *testing.T, state domain.DocDisplayStringState) []byte {
	test.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		test.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		test.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func TestDocDisplayReviewRejectsCheckpointCountingOpeningQuoteAsText(test *testing.T) {
	source, raw := docDisplayReviewSource(test, strings.Repeat("a", 100_000))
	step := docDisplayReviewStep(test, source, docDisplayReviewInitial(test, source), raw)
	malformed := step.State
	malformed.Next = source.Token.From + malformed.TextBytes
	checkpoint := docDisplayReviewCheckpoint(test, malformed)
	if decoded, err := domain.DecodeDocDisplayStringState(source, domain.HashContent(checkpoint), checkpoint); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatalf("checkpoint claims %d decoded bytes after consuming only %d content bytes: next=%d error=%v", decoded.TextBytes, decoded.Next-source.Token.From-1, decoded.Next, err)
	}
}

func TestDocDisplayReviewRejectsCompletedStagingPrefix(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		bytes int
	}{
		{"state-consumption-bound", 400_000},
		{"final-page-coverage-under-sixfold-ratio", 100_000},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			output := strings.Repeat("a", scenario.bytes)
			source, raw := docDisplayReviewSource(test, output)
			first := docDisplayReviewStep(test, source, docDisplayReviewInitial(test, source), raw)
			malformed := first.State
			malformed.Next = source.Token.To - 1
			checkpoint := docDisplayReviewCheckpoint(test, malformed)
			resumed, err := domain.DecodeDocDisplayStringState(source, domain.HashContent(checkpoint), checkpoint)
			if errors.Is(err, domain.ErrIntegrity) {
				test.Log("rejected at checkpoint decode")
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			terminal, err := domain.StepDocDisplayString(context.Background(), source, resumed, func(_ context.Context, offset, length int) ([]byte, error) {
				return raw[offset : offset+length], nil
			})
			if errors.Is(err, domain.ErrIntegrity) {
				test.Log("rejected at resumed step")
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if err := domain.ValidateDocDisplayStringStep(context.Background(), source, resumed, terminal); errors.Is(err, domain.ErrIntegrity) {
				test.Log("rejected at step commit validation")
				return
			} else if err != nil {
				test.Fatal(err)
			}
			visible, err := docDisplayReviewReadFinished(source, terminal.State, first.Objects)
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("staging prefix published as complete: visible=%d original=%d omitted=%d terminal_page_nil=%v read=%v", len(visible.Text), len(output), len(output)-len(visible.Text), terminal.Page == nil, err)
			}
			test.Log("rejected before serving completed last page")
		})
	}
}

func TestDocDisplayReviewRejectsStructuralAndNumericTampering(test *testing.T) {
	source, raw := docDisplayReviewSource(test, strings.Repeat("a", 100_000))
	prior := docDisplayReviewInitial(test, source)
	for name, mutate := range map[string]func(*domain.DocDisplayStringStep){
		"extra-object": func(step *domain.DocDisplayStringStep) { step.Objects = append(step.Objects, step.Objects[0]) },
		"substituted-object": func(step *domain.DocDisplayStringStep) {
			step.Objects[0] = domain.DocRangeObject{Hash: domain.HashContent([]byte("unrelated")), Bytes: []byte("unrelated")}
		},
		"page-index-overflow": func(step *domain.DocDisplayStringStep) { step.Page.Index = math.MaxInt },
		"page-start-overflow": func(step *domain.DocDisplayStringStep) { step.Page.From = math.MaxInt },
		"run-source-overflow": func(step *domain.DocDisplayStringStep) { step.Page.Runs[0].SourceTo = math.MaxInt },
		"next-overflow":       func(step *domain.DocDisplayStringStep) { step.State.Next = math.MaxInt },
		"next-underflow":      func(step *domain.DocDisplayStringStep) { step.State.Next = math.MinInt },
		"text-overflow":       func(step *domain.DocDisplayStringStep) { step.State.TextBytes = math.MaxInt },
		"directory-overflow":  func(step *domain.DocDisplayStringStep) { step.State.Directory.Count = math.MaxInt },
		"read-overflow":       func(step *domain.DocDisplayStringStep) { step.ReadBytes = math.MaxInt },
		"state-version":       func(step *domain.DocDisplayStringStep) { step.State.Version = 2 },
		"state-source":        func(step *domain.DocDisplayStringStep) { step.State.Source = domain.HashContent([]byte("foreign")) },
		"raw-prefix-gap":      func(step *domain.DocDisplayStringStep) { step.Page.Runs[0].SourceFrom++ },
		"decoded-prefix-gap":  func(step *domain.DocDisplayStringStep) { step.Page.From++ },
	} {
		test.Run(name, func(test *testing.T) {
			step := docDisplayReviewStep(test, source, prior, raw)
			mutate(&step)
			if err := domain.ValidateDocDisplayStringStep(context.Background(), source, prior, step); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("malformed transition accepted", err)
			}
		})
	}
}

func docDisplayReviewJob(test *testing.T, source domain.DocDisplayStringSource) domain.DocDisplayJob {
	test.Helper()
	now := time.Now().UTC()
	job := domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{
		RepoID: domain.HashContent([]byte("review-repository")), DocHash: domain.HashContent([]byte("review-document")),
		State: "running", Attempts: 2, Version: 7, CreatedAt: now, NextAttempt: now, LeaseUntil: now.Add(2 * time.Minute),
	}}
	job.Target = domain.DocDisplayTarget{RepoID: job.RepoID, DocHash: job.DocHash,
		Binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", SourceVersion: 3, RangeGeneration: domain.HashContent([]byte("review-range"))},
		Event:   domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes},
	}
	if err := job.Validate(); err != nil {
		test.Fatal(err)
	}
	return job
}

func TestDocDisplayReviewRejectsInvalidJobEnumsAndCoordinates(test *testing.T) {
	source, _ := docDisplayReviewSource(test, "value")
	valid := docDisplayReviewJob(test, source)
	for name, mutate := range map[string]func(*domain.DocDisplayJob){
		"unknown-state":       func(job *domain.DocDisplayJob) { job.State = "publishing" },
		"negative-index":      func(job *domain.DocDisplayJob) { job.Target.Event.Index = -1 },
		"offset-overflow":     func(job *domain.DocDisplayJob) { job.Target.Event.Offset = math.MaxInt },
		"length-overflow":     func(job *domain.DocDisplayJob) { job.Target.Event.Length = math.MaxInt },
		"source-incarnation":  func(job *domain.DocDisplayJob) { job.Target.Binding.SourceID = "invalid" },
		"negative-generation": func(job *domain.DocDisplayJob) { job.Target.Binding.SourceVersion = -1 },
		"foreign-repository":  func(job *domain.DocDisplayJob) { job.Target.RepoID = domain.HashContent([]byte("foreign")) },
		"foreign-document":    func(job *domain.DocDisplayJob) { job.Target.DocHash = domain.HashContent([]byte("foreign")) },
		"unexpected-base":     func(job *domain.DocDisplayJob) { job.BaseHash = job.DocHash },
	} {
		test.Run(name, func(test *testing.T) {
			job := valid
			mutate(&job)
			if err := job.Validate(); !errors.Is(err, domain.ErrValidation) {
				test.Fatal("invalid job accepted", err)
			}
		})
	}
	target := valid.Target
	target.Event.Index = math.MaxInt
	if target.Validate() == nil {
		test.Fatal("positive out-of-domain index passes shape validation")
	}
}
