package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayCompletionStore struct {
	outbound.DocDisplayStore
	job                           domain.DocDisplayJob
	preparation                   domain.DocDisplayPreparation
	loadError, completeError      error
	claims, completions, finishes int
	finished                      domain.DocDisplayJob
}

func (storage *docDisplayCompletionStore) ClaimDocDisplay(context.Context, time.Time, time.Duration) (domain.DocDisplayJob, error) {
	storage.claims++
	if storage.claims > 1 {
		return domain.DocDisplayJob{}, domain.ErrNotFound
	}
	return storage.job, nil
}

func (storage *docDisplayCompletionStore) LoadDocDisplayJob(context.Context, domain.DocDisplayJob) (domain.DocDisplayPreparation, error) {
	return storage.preparation, storage.loadError
}

func (storage *docDisplayCompletionStore) CompleteDocDisplay(context.Context, domain.DocDisplayJob, domain.ContentHash, time.Time) error {
	storage.completions++
	return storage.completeError
}

func (storage *docDisplayCompletionStore) FinishDocDisplay(_ context.Context, job domain.DocDisplayJob, _ time.Time) error {
	storage.finishes++
	storage.finished = job
	return nil
}

func TestDocDisplayWorkerCompletionErrorsDoNotBecomeSuccess(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"value"}`)
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	initial, err := domain.NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := domain.StepDocDisplayString(context.Background(), source, initial, func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length], nil })
	if err != nil {
		test.Fatal(err)
	}
	checkpoint, err := step.State.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC()
	job := domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{
		RepoID: domain.HashContent([]byte("completion-repo")), DocHash: domain.HashContent([]byte("completion-document")), State: "running",
		CreatedAt: now, NextAttempt: now, LeaseUntil: now.Add(time.Minute),
	}}
	job.Target = domain.DocDisplayTarget{RepoID: job.RepoID, DocHash: job.DocHash,
		Binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent([]byte("generation"))},
		Event:   domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes},
	}
	for _, scenario := range []struct {
		name, state, reason string
		failure             error
		atLoad              bool
	}{
		{"success", "", "", nil, false},
		{"transient-complete", "retrying", "temporary_failure", errors.New("commit unavailable"), false},
		{"changed-at-complete", "rejected", "source_changed", domain.ErrConflict, false},
		{"changed-at-load", "rejected", "source_changed", domain.ErrConflict, true},
		{"missing-source", "rejected", "source_unavailable", domain.ErrNotFound, false},
		{"invalid-source", "rejected", "invalid_document", domain.ErrIntegrity, false},
		{"interrupted-complete", "retrying", "verification_interrupted", context.Canceled, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage := &docDisplayCompletionStore{job: job, preparation: domain.DocDisplayPreparation{Source: &source, State: step.State, Checkpoint: domain.HashContent(checkpoint)}}
			if scenario.atLoad {
				storage.loadError = scenario.failure
			} else {
				storage.completeError = scenario.failure
			}
			service := NewService(nil, nil, nil, nil, nil)
			service.ConfigureDocumentDisplay(storage)
			if err := service.ProcessDocDisplayJobs(context.Background(), 1); !errors.Is(err, scenario.failure) {
				test.Fatal("worker dropped completion error", err)
			}
			if scenario.failure == nil {
				if storage.completions != 1 || storage.finishes != 0 {
					test.Fatal(storage)
				}
			} else if storage.finishes != 1 || storage.finished.State != scenario.state || storage.finished.Reason != scenario.reason {
				test.Fatal("failed completion reported as success or lost retry reason", storage.finished)
			}
		})
	}
}

type docDisplayDrainStore struct {
	*docDisplayCompletionStore
	quota int
	done  chan struct{}
}

func (storage *docDisplayDrainStore) ClaimDocDisplay(context.Context, time.Time, time.Duration) (domain.DocDisplayJob, error) {
	if storage.claims == storage.quota {
		return domain.DocDisplayJob{}, domain.ErrNotFound
	}
	storage.claims++
	job := storage.job
	job.Version = int64(storage.claims)
	return job, nil
}

func (storage *docDisplayDrainStore) CompleteDocDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	err := storage.docDisplayCompletionStore.CompleteDocDisplay(ctx, job, checkpoint, now)
	if storage.completions == storage.quota {
		close(storage.done)
	}
	return err
}

func TestDocDisplayWorkerDrainsReadyClaimsWithoutPollingSleep(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":" "}`)
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	state, err := domain.NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC()
	job := domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte("drain repo")), DocHash: domain.HashContent([]byte("drain doc")), State: "running", CreatedAt: now, NextAttempt: now, LeaseUntil: now.Add(time.Minute)}}
	job.Target = domain.DocDisplayTarget{RepoID: job.RepoID, DocHash: job.DocHash,
		Binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent([]byte("range"))}, Event: domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes}}
	storage := &docDisplayDrainStore{docDisplayCompletionStore: &docDisplayCompletionStore{job: job, preparation: domain.DocDisplayPreparation{Source: &source, State: state, Checkpoint: domain.HashContent(checkpoint)}}, quota: 10, done: make(chan struct{})}
	service := NewService(nil, nil, nil, nil, nil)
	service.ConfigureDocumentDisplay(storage)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		service.RunDocDisplayWorker(ctx)
	}()
	select {
	case <-storage.done:
		cancel()
		<-stopped
	case <-ctx.Done():
		<-stopped
		test.Fatal("ready work was throttled by idle polling sleeps", storage.completions)
	}
}
