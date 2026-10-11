//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func conditionalAppendPGFixture(t *testing.T) (historyMemorySelectionFixture, *store.PostgresStore, inbound.UpdateRefInput) {
	t.Helper()
	service, storage, repo := collaborationPG(t)
	fixture := newHistoryMemorySelectionFixture(t, service, storage, repo)
	revision, err := storage.RepositoryRevision(fixture.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	input := inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: fixture.before.BranchID, Target: fixture.other}, Append: true, ExpectedTarget: fixture.before.Target, ExpectedRevision: &revision}
	return fixture, storage, input
}

func conditionalAppendPGState(t *testing.T, fixture historyMemorySelectionFixture, storage *store.PostgresStore) string {
	t.Helper()
	refs, err := storage.ListRefs(fixture.ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.other)
	if err != nil {
		t.Fatal(err)
	}
	history, err := storage.ListHistoryEvents(fixture.ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	reflog, err := storage.ReadReflog(fixture.ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{refs, snapshot, history, reflog, revision})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPGConditionalAppendFences(t *testing.T) {
	for _, mode := range []string{"graph", "evidence", "wrong target", "same requested target", "branch identity", "missing target", "force", "not append", "revoked", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			fixture, storage, input := conditionalAppendPGFixture(t)
			service := fixture.svc
			want := domain.ErrRefConflict
			switch mode {
			case "graph":
				if err := storage.AdvanceRepositoryRevision(fixture.ctx, fixture.repo, false); err != nil {
					t.Fatal(err)
				}
			case "evidence":
				if err := storage.AdvanceEvidenceRevision(fixture.ctx, fixture.repo); err != nil {
					t.Fatal(err)
				}
			case "wrong target":
				input.ExpectedTarget = fixture.other
			case "same requested target":
				input.Ref.Target, input.ExpectedTarget = fixture.before.Target, fixture.other
			case "branch identity":
				input.Ref.BranchID = "stale-context-identity"
			case "missing target":
				input.ExpectedTarget, want = "", domain.ErrIntegrity
			case "force":
				input.Force, want = true, domain.ErrValidation
			case "not append":
				input.Append, want = false, domain.ErrValidation
			case "revoked":
				if err := storage.WithinIdentity(fixture.ctx, func(bound context.Context) error {
					return storage.AddMember(bound, domain.Membership{RepositoryID: fixture.repository, UserID: fixture.member, Role: domain.RoleViewer})
				}); err != nil {
					t.Fatal(err)
				}
				input.ExpectedRevision.Graph++
				want = domain.ErrForbidden
			case "rollback":
				broken := conditionalAppendRevisionFailure{PostgresStore: storage}
				service = NewService(broken, broken, nil, gitengine.NewEngine(broken), broken)
				want = domain.ErrIntegrity
			}
			before := conditionalAppendPGState(t, fixture, storage)
			if _, err := service.UpdateRef(fixture.ctx, input); !errors.Is(err, want) {
				t.Fatalf("conditional append error=%v want=%v", err, want)
			}
			if after := conditionalAppendPGState(t, fixture, storage); after != before {
				t.Fatalf("failed conditional append changed repository:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

type conditionalAppendRevisionFailure struct{ *store.PostgresStore }

func (storage conditionalAppendRevisionFailure) AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error {
	return domain.ErrIntegrity
}

func TestPGConditionalAppendIgnoresPendingRevision(t *testing.T) {
	fixture, storage, input := conditionalAppendPGFixture(t)
	if err := storage.AdvanceRepositoryRevision(fixture.ctx, fixture.repo, true); err != nil {
		t.Fatal(err)
	}
	output, err := fixture.svc.UpdateRef(fixture.ctx, input)
	if err != nil || output.Result != inbound.RefAppended {
		t.Fatalf("pending-only churn rejected: %+v %v", output, err)
	}
	ref, err := storage.GetRef(fixture.ctx, fixture.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != fixture.other {
		t.Fatalf("ref: %+v %v", ref, err)
	}
	snapshot, err := storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.other)
	if err != nil || len(snapshot.Parents) != 0 || len(snapshot.GraftParents) != 1 || snapshot.GraftParents[0] != fixture.before.Target {
		t.Fatalf("append failed to preserve natural history: %+v %v", snapshot, err)
	}
	revision, err := storage.RepositoryRevision(fixture.ctx, fixture.repo)
	if err != nil || revision.Graph != input.ExpectedRevision.Graph+1 || revision.Evidence != input.ExpectedRevision.Evidence || revision.Pending != input.ExpectedRevision.Pending+1 {
		t.Fatalf("revision: %+v %v", revision, err)
	}
	if _, err := fixture.svc.UpdateRef(fixture.ctx, input); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("stale same-target retry bypassed revision: %v", err)
	}
	input.ExpectedRevision, input.ExpectedTarget = nil, fixture.before.Target
	if _, err := fixture.svc.UpdateRef(fixture.ctx, input); err != nil {
		t.Fatalf("legacy same-target retry changed: %v", err)
	}
}

type conditionalAppendEnteringStore struct {
	*store.PostgresStore
	entered chan struct{}
}

func (storage conditionalAppendEnteringStore) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	close(storage.entered)
	return storage.PostgresStore.WithinRepository(ctx, repo, fn)
}

func TestPGConditionalAppendWaitsForSemanticWriter(t *testing.T) {
	for _, scope := range []string{"graph", "evidence"} {
		t.Run(scope, func(t *testing.T) {
			fixture, storage, input := conditionalAppendPGFixture(t)
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			locked, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			contribution := (domain.PRContribution{RepoID: fixture.repo, GitOrigin: "https://github.com/acme/proj", ProviderRepositoryID: 1, PullRequestID: 492, PR: domain.PullRequestMerge{Number: 492, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}, DestinationBranchID: input.Ref.BranchID, SourceBranchID: "feature-id", Capture: fixture.other, OriginalHead: strings.Repeat("a", 40)}).WithID()
			go func() {
				firstDone <- storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
					if err := storage.PutPRContribution(bound, contribution); err != nil {
						return err
					}
					var err error
					if scope == "evidence" {
						err = storage.AdvanceEvidenceRevision(bound, fixture.repo)
					} else {
						err = storage.AdvanceRepositoryRevision(bound, fixture.repo, false)
					}
					if err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-locked:
			case err := <-firstDone:
				t.Fatal("writer failed before lock", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			probe := conditionalAppendEnteringStore{PostgresStore: storage, entered: make(chan struct{})}
			service := NewService(probe, probe, nil, gitengine.NewEngine(probe), probe)
			appendDone := make(chan error, 1)
			go func() { _, err := service.UpdateRef(ctx, input); appendDone <- err }()
			select {
			case <-probe.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			if err := <-appendDone; !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("late semantic commit bypassed CAS: %v", err)
			}
			ref, err := storage.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
			if err != nil || ref.Target != input.ExpectedTarget {
				t.Fatalf("raw branch moved: %+v %v", ref, err)
			}
			snapshot, err := storage.GetSnapshot(ctx, fixture.repo, fixture.other)
			if err != nil || len(snapshot.GraftParents) != 0 {
				t.Fatalf("rejected append grafted source: %+v %v", snapshot, err)
			}
			rows, err := storage.ListPRContributions(ctx, fixture.repo)
			if err != nil || len(rows) != 1 || rows[0].ID != contribution.ID {
				t.Fatalf("semantic writer lost: %+v %v", rows, err)
			}
		})
	}
}

func TestPGConditionalAppendConcurrentSamePlan(t *testing.T) {
	fixture, _, input := conditionalAppendPGFixture(t)
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { <-start; _, err := fixture.svc.UpdateRef(ctx, input); results <- err })
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrRefConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("successes=%d conflicts=%d", succeeded, conflicted)
	}
}
