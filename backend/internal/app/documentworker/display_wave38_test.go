package documentworker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave38DisplayStore struct {
	outbound.DocDisplayStore
	scope                           *wave38DisplayScope
	job                             domain.DocDisplayJob
	progress                        domain.DocDisplayPreparation
	loadError, completeError        error
	claims, quota                   int
	discoveries, steps, completions int
	finished                        []domain.DocDisplayJob
	cancel                          context.CancelFunc
	onComplete                      func()
}

func (storage *wave38DisplayStore) ClaimDocDisplay(ctx context.Context, _ time.Time, lease time.Duration) (domain.DocDisplayJob, error) {
	if storage.scope.activeWork != 1 || ctx.Value(wave38DisplayWorkKey{}) != true || lease != Lease {
		storage.scope.test.Fatal("display claim escaped work admission or changed lease")
	}
	storage.claims++
	if storage.claims > storage.quota {
		return domain.DocDisplayJob{}, domain.ErrNotFound
	}
	return storage.job, nil
}

func (storage *wave38DisplayStore) LoadDocDisplayJob(ctx context.Context, _ domain.DocDisplayJob) (domain.DocDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	if storage.cancel != nil {
		storage.cancel()
	}
	return storage.progress, storage.loadError
}

func (storage *wave38DisplayStore) InitializeDocDisplay(ctx context.Context, _ domain.DocDisplayJob, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	state, err := domain.NewDocDisplayStringState(source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	encoded, err := state.Encode(source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	storage.progress = domain.DocDisplayPreparation{Source: &source, State: state, Checkpoint: domain.HashContent(encoded)}
	return storage.progress, nil
}

func (storage *wave38DisplayStore) SaveDocDisplayDiscovery(ctx context.Context, job domain.DocDisplayJob, step domain.DocFieldDiscoveryStep) (domain.DocDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	state, err := step.State()
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	storage.discoveries++
	if state.Done {
		source, err := state.DisplaySource()
		if err != nil {
			return domain.DocDisplayPreparation{}, err
		}
		return storage.InitializeDocDisplay(ctx, job, source)
	}
	checkpoint, _ := step.Checkpoint()
	storage.progress = domain.DocDisplayPreparation{Discovery: &state, DiscoveryCheckpoint: checkpoint}
	return storage.progress, nil
}

func (storage *wave38DisplayStore) SaveDocDisplayStep(ctx context.Context, _ domain.DocDisplayJob, checkpoint domain.ContentHash, step domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	if checkpoint != storage.progress.Checkpoint {
		return domain.DocDisplayPreparation{}, domain.ErrConflict
	}
	encoded, err := step.State.Encode(*storage.progress.Source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	storage.steps++
	storage.progress.State, storage.progress.Checkpoint = step.State, domain.HashContent(encoded)
	return storage.progress, nil
}

func (storage *wave38DisplayStore) CompleteDocDisplay(ctx context.Context, _ domain.DocDisplayJob, checkpoint domain.ContentHash, _ time.Time) error {
	storage.scope.requireWrite(ctx)
	if !storage.progress.State.Done || checkpoint != storage.progress.Checkpoint {
		storage.scope.test.Fatal("display completed without the final checkpoint")
	}
	storage.completions++
	if storage.onComplete != nil {
		storage.onComplete()
	}
	return storage.completeError
}

func (storage *wave38DisplayStore) FinishDocDisplay(ctx context.Context, job domain.DocDisplayJob, _ time.Time) error {
	storage.scope.requireWrite(ctx)
	deadline, bounded := ctx.Deadline()
	if ctx.Err() != nil || !bounded || time.Until(deadline) > 5*time.Second {
		storage.scope.test.Fatal("display finish lost independent bounded context")
	}
	storage.finished = append(storage.finished, job)
	return nil
}

func newWave38CompletedDisplayStore(test *testing.T) *wave38DisplayStore {
	test.Helper()
	raw := []byte(`{"kind":"tool_result","output":" "}`)
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	storage := &wave38DisplayStore{scope: &wave38DisplayScope{test: test}, quota: 1}
	storage.job = domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte("display repo")), DocHash: domain.HashContent([]byte("display doc")), Attempts: 10},
		Target: domain.DocDisplayTarget{Event: domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes}}}
	if _, err := storage.InitializeDocDisplay(context.Background(), storage.job, source); err != nil {
		test.Fatal(err)
	}
	return storage
}

func TestWave38DisplayOwnerDiscoveryBudgetAndDurableResume(test *testing.T) {
	fixture, queries := newWave38DisplayFixture(test, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("public ", 80_000)}})
	sources := &wave38DisplaySources{queries: queries, scope: fixture.scope}
	storage := &wave38DisplayStore{scope: fixture.scope, job: domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash}, Target: fixture.target(0, "")}}
	worker := NewDisplayWorker(storage, sources, fixture.scope)
	published, err := worker.Build(context.Background(), storage.job)
	if err != nil || published || storage.discoveries != DisplayJobSteps || storage.steps != 0 || fixture.scope.reads != DisplayJobSteps ||
		storage.progress.Discovery == nil || storage.progress.Discovery.Next != DisplayJobSteps*domain.DocFieldDiscoveryStepBytes {
		test.Fatal("display discovery lost its eight-step bounded continuation", published, err, storage)
	}
	for attempt := 0; attempt < 40 && !published; attempt++ {
		before := fixture.scope.reads
		published, err = NewDisplayWorker(storage, sources, fixture.scope).Build(context.Background(), storage.job)
		if err != nil || fixture.scope.reads-before > DisplayJobSteps {
			test.Fatal("resumed display exceeded step budget or failed", err)
		}
	}
	if !published || storage.completions != 1 || storage.steps == 0 || sources.view != 0 || sources.display != fixture.scope.reads || fixture.metadata != fixture.scope.reads {
		test.Fatal("display did not resume through its actual string projection", published, storage, sources)
	}
	if _, err := sources.last.Read(0, 1); !errors.Is(err, context.Canceled) {
		test.Fatal("display retained a source after its snapshot closed", err)
	}
}

func TestWave38DisplayOwnerSourceBindingEntryKindAndValidationOrder(test *testing.T) {
	fixture, queries := newWave38DisplayFixture(test, []domain.CIREvent{{Kind: domain.EventToolResult, Output: "value"}, {Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "message"}}}})
	worker := NewDisplayWorker(nil, queries, fixture.scope)
	for _, scenario := range []struct {
		name   string
		target domain.DocDisplayTarget
		want   error
	}{
		{"string", fixture.target(0, ""), nil},
		{"projection", fixture.target(1, domain.DocDisplayProjectionProfile), nil},
		{"string-wrong-kind", fixture.target(1, ""), domain.ErrIntegrity},
		{"invalid", domain.DocDisplayTarget{}, domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			err := fixture.scope.WithinRepositoryRead(context.Background(), fixture.repo, func(read context.Context) error {
				_, err := worker.Source(read, scenario.target)
				return err
			})
			if !errors.Is(err, scenario.want) {
				test.Fatal("display source validation changed", err, scenario.want)
			}
		})
	}
	for _, bindingChange := range []bool{true, false} {
		target := fixture.target(0, "")
		want := domain.ErrIntegrity
		if bindingChange {
			target.Binding.SourceVersion++
			want = domain.ErrConflict
		} else {
			target.Event.Hash = domain.HashContent([]byte("another event"))
		}
		before := fixture.pageReads
		err := fixture.scope.WithinRepositoryRead(context.Background(), fixture.repo, func(read context.Context) error {
			_, err := worker.Source(read, target)
			return err
		})
		if !errors.Is(err, want) || bindingChange && fixture.pageReads != before {
			test.Fatal("display changed binding-before-entry validation", err, fixture.pageReads-before)
		}
	}
	if _, err := NewDisplayWorker(nil, nil, nil).Source(context.Background(), domain.DocDisplayTarget{}); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("invalid display target reached missing dependencies", err)
	}
}

func TestWave38DisplayOwnerRejectsMixedCheckpointsBeforeReading(test *testing.T) {
	storage := newWave38CompletedDisplayStore(test)
	checkpoint := domain.HashContent([]byte("discovery checkpoint"))
	state, err := domain.NewDocFieldDiscoveryState(storage.job.Target.Event.Hash, storage.job.Target.Event.Length)
	if err != nil {
		test.Fatal(err)
	}
	worker := NewDisplayWorker(storage, nil, storage.scope)
	for _, progress := range []domain.DocDisplayPreparation{
		{Checkpoint: checkpoint},
		{Discovery: &state},
		{DiscoveryCheckpoint: checkpoint},
	} {
		if _, err := worker.AdvanceDiscovery(context.Background(), storage.job, progress); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("mixed discovery checkpoint reached source dependencies", progress, err)
		}
	}
	storage.progress.Discovery = &state
	if published, err := worker.Build(context.Background(), storage.job); published || !errors.Is(err, domain.ErrIntegrity) || storage.scope.reads != 0 || storage.completions != 0 {
		test.Fatal("completed string checkpoint ignored mixed discovery state", published, err)
	}
}

func TestWave38DisplayOwnerCompletionRecoveryAndPublishedFlag(test *testing.T) {
	for _, scenario := range []struct {
		name, state, reason string
		failure             error
	}{
		{"success", "", "", nil},
		{"missing", "rejected", "source_unavailable", domain.ErrNotFound},
		{"incarnation", "retrying", "source_incarnation_changed", domain.ErrDocDisplayIncarnationChanged},
		{"binding", "rejected", "source_changed", domain.ErrConflict},
		{"unsupported", "rejected", "unsupported_display", domain.ErrDocFindDisplayUnavailable},
		{"integrity", "rejected", "invalid_document", domain.ErrIntegrity},
		{"validation", "rejected", "invalid_document", domain.ErrValidation},
		{"interrupted", "retrying", "verification_interrupted", context.Canceled},
		{"deadline", "retrying", "verification_interrupted", context.DeadlineExceeded},
		{"temporary", "retrying", "temporary_failure", errors.New("publish failed")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage := newWave38CompletedDisplayStore(test)
			storage.completeError = scenario.failure
			worker := NewDisplayWorker(storage, nil, storage.scope)
			published, err := worker.Build(context.Background(), storage.job)
			if !published || !errors.Is(err, scenario.failure) {
				test.Fatal("completion result lost its published flag or failure", published, err)
			}
			err = worker.RunJob(context.Background(), storage.job)
			if !errors.Is(err, scenario.failure) {
				test.Fatal("display dropped a completion failure", err)
			}
			if scenario.failure == nil {
				if len(storage.finished) != 0 {
					test.Fatal("successful publication was finished twice", storage.finished)
				}
				return
			}
			if len(storage.finished) != 1 || storage.finished[0].State != scenario.state || storage.finished[0].Reason != scenario.reason || !storage.finished[0].LeaseUntil.IsZero() || storage.finished[0].NextAttempt.Before(time.Now().Add(250*time.Second)) {
				test.Fatal("display lost classification, lease release, or capped retry", storage.finished)
			}
		})
	}
	storage := newWave38CompletedDisplayStore(test)
	ctx, cancel := context.WithCancel(context.Background())
	storage.cancel, storage.loadError = cancel, context.Canceled
	if err := NewDisplayWorker(storage, nil, storage.scope).RunJob(ctx, storage.job); !errors.Is(err, context.Canceled) || len(storage.finished) != 0 {
		test.Fatal("parent cancellation finalized display work", err, storage.finished)
	}
}

func TestWave38DisplayOwnerAdmissionCountersLimitsAndProductiveLoop(test *testing.T) {
	storage := newWave38CompletedDisplayStore(test)
	storage.scope.workError = domain.ErrResourceOverloaded
	worker := NewDisplayWorker(storage, nil, storage.scope)
	if processed, err := worker.Process(context.Background(), 1); processed != 0 || !errors.Is(err, domain.ErrResourceOverloaded) || storage.claims != 0 {
		test.Fatal("display claimed before admission", processed, err, storage.claims)
	}
	storage.scope.workError = nil
	storage.quota = 2
	if processed, err := worker.Process(context.Background(), 4); processed != 2 || err != nil || storage.claims != 3 || storage.completions != 2 {
		test.Fatal("display lost productive counters or queue exhaustion", processed, err, storage)
	}
	if processed, err := worker.Process(context.Background(), 0); processed != 0 || err != nil || storage.claims != 3 {
		test.Fatal("nonpositive display limit claimed work", processed, err)
	}
	if processed, err := NewDisplayWorker(nil, nil, nil).Process(context.Background(), 1); processed != 0 || err != nil {
		test.Fatal("nil display storage stopped being a no-op", processed, err)
	}
	storage = newWave38CompletedDisplayStore(test)
	storage.loadError = domain.ErrNotFound
	if processed, err := NewDisplayWorker(storage, nil, storage.scope).Process(context.Background(), 1); processed != 0 || !errors.Is(err, domain.ErrNotFound) || len(storage.finished) != 1 {
		test.Fatal("claimed missing source was mistaken for empty queue", processed, err)
	}
	storage = newWave38CompletedDisplayStore(test)
	storage.quota = 5
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	storage.onComplete = func() {
		if storage.completions == storage.quota {
			cancel()
		}
	}
	NewDisplayWorker(storage, nil, storage.scope).Run(ctx)
	if storage.completions != storage.quota {
		test.Fatal("ready display work was delayed by idle polling", storage.completions)
	}
}
