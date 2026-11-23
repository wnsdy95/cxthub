package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type historyOwnerCallerKey struct{}
type historyOwnerScopeKey struct{}
type historyOwnerProofKey struct{}

type historyOwnerProbe struct {
	test         *testing.T
	ctx          context.Context
	repository   domain.Repo
	snapshot     func(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	memory       func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	verify       func(context.Context, domain.ContentHash, domain.Snapshot) error
	accepted     []domain.HistoryEvent
	written      []domain.HistoryEvent
	queued       []domain.HistoryEvent
	captured     []domain.Snapshot
	notification domain.Ref
	calls        []string
	failures     map[string]error
	active       string
	preparing    bool
	nilProof     bool
}

func (probe *historyOwnerProbe) caller(ctx context.Context) {
	probe.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	wantActor, wantSystem := inbound.RepositoryActor(probe.ctx)
	deadline, hasDeadline := ctx.Deadline()
	wantDeadline, wantHasDeadline := probe.ctx.Deadline()
	if ctx.Value(historyOwnerCallerKey{}) != probe || actor != wantActor || system != wantSystem ||
		inbound.Correlation(ctx) != "history-owner" || hasDeadline != wantHasDeadline || !deadline.Equal(wantDeadline) {
		probe.test.Fatal("history owner discarded caller context")
	}
	if probe.preparing && ctx.Value(historyOwnerProofKey{}) != probe {
		probe.test.Fatal("history owner discarded document-proof context")
	}
}

func (probe *historyOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.test.Helper()
	probe.caller(ctx)
	if repo != probe.repository.ID || probe.active == "" || ctx.Value(historyOwnerScopeKey{}) != probe.active {
		probe.test.Fatalf("%s escaped bound repository scope %q", operation, probe.active)
	}
	probe.calls = append(probe.calls, operation)
	if err := ctx.Err(); err != nil {
		return err
	}
	return probe.failures[operation]
}

func (probe *historyOwnerProbe) count(operation string) int {
	count := 0
	for _, actual := range probe.calls {
		if actual == operation {
			count++
		}
	}
	return count
}

func (probe *historyOwnerProbe) require(condition bool, message string) {
	probe.test.Helper()
	if !condition {
		probe.test.Fatalf("%s: calls=%v", message, probe.calls)
	}
}

func (probe *historyOwnerProbe) result(actual, wanted error) {
	probe.test.Helper()
	if !errors.Is(actual, wanted) {
		probe.test.Fatalf("err=%v want=%v calls=%v", actual, wanted, probe.calls)
	}
}

func (probe *historyOwnerProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return probe.repository, probe.touch(ctx, repo, "repo")
}

func (probe *historyOwnerProbe) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := probe.touch(ctx, repo, "snapshot"); err != nil {
		return domain.Snapshot{}, err
	}
	return probe.snapshot(ctx, repo, hash)
}

func (probe *historyOwnerProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return append([]domain.HistoryEvent(nil), probe.accepted...), probe.touch(ctx, repo, "history")
}

func (probe *historyOwnerProbe) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if probe.active != "write" {
		probe.test.Fatal("history mutation ran outside final write")
	}
	if err := probe.touch(ctx, domain.ContentHash(event.RepoID), "apply"); err != nil {
		return err
	}
	probe.written = append(probe.written, event)
	probe.accepted = append(probe.accepted, event)
	return nil
}

func (probe *historyOwnerProbe) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	probe.require(probe.active == "write", "memory read escaped final write")
	if err := probe.touch(ctx, repo, "memory"); err != nil {
		return domain.MemoryDigest{}, err
	}
	return probe.memory(ctx, repo, hash)
}

func (probe *historyOwnerProbe) VerifySnapshotDocument(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	wantScope := "write"
	if probe.preparing && !probe.nilProof {
		wantScope = "read"
	}
	if probe.active != wantScope {
		probe.test.Fatalf("document verification ran in %q, want %q", probe.active, wantScope)
	}
	if err := probe.touch(ctx, repo, "verify"); err != nil {
		return err
	}
	return probe.verify(ctx, repo, snapshot)
}

func (probe *historyOwnerProbe) within(ctx context.Context, repo domain.ContentHash, kind string, operation func(context.Context) error) error {
	probe.caller(ctx)
	if probe.active != "" || repo != probe.repository.ID {
		probe.test.Fatal("history owner nested or changed repository scope")
	}
	probe.calls = append(probe.calls, kind)
	if err := probe.failures[kind]; err != nil {
		return err
	}
	probe.active = kind
	defer func() { probe.active = "" }()
	return operation(context.WithValue(ctx, historyOwnerScopeKey{}, kind))
}

func (probe *historyOwnerProbe) WithinHistoryWrite(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "write", operation)
}

func (probe *historyOwnerProbe) WithinHistoryObservation(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "observation", operation)
}

func (probe *historyOwnerProbe) WithinHistoryRead(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "read", operation)
}

func (probe *historyOwnerProbe) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	probe.require(probe.active == "write", "notification escaped final write")
	if !forced || created || ref.RepoID != repo || ref.Kind != domain.RefBranch {
		probe.test.Fatal("history advance changed notification semantics")
	}
	probe.notification = ref
	return probe.touch(ctx, repo, "notify")
}

func (probe *historyOwnerProbe) QueueHistoryGitScan(ctx context.Context, event domain.HistoryEvent) error {
	probe.require(probe.active == "write", "Git scan escaped final write")
	probe.queued = append(probe.queued, event)
	return probe.touch(ctx, domain.ContentHash(event.RepoID), "scan")
}

func (probe *historyOwnerProbe) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, when time.Time) error {
	probe.require(probe.active == "write", "PR wake escaped final write")
	if when.IsZero() {
		probe.test.Fatal("publication wake lost its timestamp")
	}
	return probe.touch(ctx, repo, "wake")
}

func (probe *historyOwnerProbe) Capture(ctx context.Context, snapshots []domain.Snapshot) error {
	if probe.active != "read" {
		probe.test.Fatal("document proof captured outside readonly preparation")
	}
	probe.captured = append([]domain.Snapshot(nil), snapshots...)
	return probe.touch(ctx, probe.repository.ID, "capture")
}

func (probe *historyOwnerProbe) Pin(ctx context.Context) error {
	if probe.active != "write" || len(probe.captured) == 0 {
		probe.test.Fatal("document proof pinned without capture or final write")
	}
	return probe.touch(ctx, probe.repository.ID, "pin")
}

var ()
