package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type streamingPreparationMetadata struct {
	preparationMetadata
	afterRead error
}

func (metadata streamingPreparationMetadata) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if err := read(ctx); err != nil {
		return err
	}
	return metadata.afterRead
}

func (metadata streamingPreparationMetadata) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	return write(ctx)
}

type streamingPreparationStore struct {
	*preparationJobStore
	prepareRoot func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error)
}

func (storage streamingPreparationStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	return storage.prepareRoot(ctx, job)
}

type streamingPreparationPublication struct {
	completeCalls int
	closeCalls    int
	completeError error
}

func (publication *streamingPreparationPublication) Complete(context.Context, domain.DocFinalizationJob, time.Time) error {
	publication.completeCalls++
	return publication.completeError
}

func (publication *streamingPreparationPublication) Close() error {
	publication.closeCalls++
	return nil
}

func TestDocJobStreamingPreparationClosesOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "read-commit-failure", "preparation-failure", "completion-failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			representation, _ := rootWorkerFixture(t)
			job, err := domain.NewDocFinalizationJobForRepresentation(hh(t.Name()), representation, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			job = job.Claim(time.Now().UTC(), docJobLease)
			failure := errors.New(outcome)
			metadata := streamingPreparationMetadata{}
			publication := &streamingPreparationPublication{}
			if outcome == "read-commit-failure" {
				metadata.afterRead = failure
			}
			if outcome == "completion-failure" {
				publication.completeError = failure
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finishCalls := 0
			storage := streamingPreparationStore{
				preparationJobStore: &preparationJobStore{
					prepare: func(context.Context, domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
						t.Fatal("streaming job retained a whole-document proof")
						return nil, nil
					},
					finish: func(_ context.Context, got domain.DocFinalizationJob, now time.Time) error {
						finishCalls++
						assertPreparationFailure(t, job, got, now, "retrying", "temporary_failure")
						return nil
					},
				},
				prepareRoot: func(_ context.Context, got domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
					if got.ID != job.ID || got.Version != job.Version {
						t.Fatal("preparation changed the claim")
					}
					if outcome == "cancellation" {
						cancel()
					}
					if outcome == "preparation-failure" {
						return publication, failure
					}
					return publication, nil
				},
			}
			service := NewService(metadata, nil, nil, nil, nil)
			err = service.runDocJob(ctx, storage, job)
			wantComplete, wantFinish := 0, 1
			switch outcome {
			case "success":
				wantComplete, wantFinish = 1, 0
				if err != nil {
					t.Fatal(err)
				}
			case "completion-failure":
				wantComplete = 1
			case "cancellation":
				wantFinish = 0
				failure = context.Canceled
			}
			if outcome != "success" && !errors.Is(err, failure) {
				t.Fatal("wrong failure", err)
			}
			if publication.closeCalls != 1 || publication.completeCalls != wantComplete || finishCalls != wantFinish {
				t.Fatalf("close/complete/finish=%d/%d/%d want 1/%d/%d", publication.closeCalls, publication.completeCalls, finishCalls, wantComplete, wantFinish)
			}
		})
	}
}
