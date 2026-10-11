//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type noWriteHistoryPG struct {
	*gitContributionApplyPGStore
	readCalls  atomic.Int64
	writeCalls atomic.Int64
}

func (storage *noWriteHistoryPG) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if !storage.InReadOnlyTransaction(ctx) {
		storage.writeCalls.Add(1)
		return nil, domain.ErrIntegrity
	}
	storage.readCalls.Add(1)
	return storage.PostgresStore.ListHistoryEvents(ctx, repo)
}

func TestPGGitAdmissionGenerationEliminatesWriterHistoryScan(t *testing.T) {
	t.Run("standalone-admission", func(t *testing.T) {
		fixture := newAdmissionPGFixture(t)
		guard := &noWriteHistoryPG{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: &admissionBatchPG{PostgresStore: fixture.storage}}}
		fixture.worker.core.meta = guard
		if _, err := fixture.finish(systemTestContext(), false); err != nil {
			t.Fatal(err)
		}
		if guard.readCalls.Load() != 0 || guard.writeCalls.Load() != 0 {
			t.Fatal("standalone admission rebuilt whole history")
		}
	})
	t.Run("reconciliation", func(t *testing.T) {
		scans, fixture, storage, _, _ := admissionBatchReconcileFixture(t, 1)
		guard := &noWriteHistoryPG{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: storage}}
		fixture.worker.core.meta = guard
		if err := scans.Reconcile(systemTestContext()); err != nil {
			t.Fatal(err)
		}
		admissions, err := guard.ListGitContributionAdmissions(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
		if err != nil || len(admissions) != 1 || guard.readCalls.Load() != 0 || guard.writeCalls.Load() != 0 {
			t.Fatal("reconciliation scanned history in writer or lost admission", len(admissions), guard.readCalls.Load(), guard.writeCalls.Load(), err)
		}
	})
	t.Run("integration", func(t *testing.T) {
		fixture := newGitContributionApplyPGFixture(t, "pinned")
		fixture.observeIntegration(t, "main", true)
		guard := &noWriteHistoryPG{gitContributionApplyPGStore: fixture.storage}
		fixture.scans.core.meta = guard
		prepared, authorization := fixture.prepare(t)
		if prepared.snapshot == nil || prepared.snapshot.generation == nil {
			t.Fatal("preparation lacks committed generation")
		}
		before := fixture.state(t, systemTestContext())
		guard.readCalls.Store(0)
		if _, err := fixture.scans.applyGitContribution(systemTestContext(), prepared, authorization); err != nil {
			t.Fatal(err)
		}
		if guard.readCalls.Load() != 0 || guard.writeCalls.Load() != 0 {
			t.Fatal("application rebuilt whole history")
		}
		assertGitContributionAppliedOnce(t, before, fixture.state(t, systemTestContext()), prepared)
	})
	t.Run("memory", func(t *testing.T) {
		fixture, storage := newSupplementPGFixture(t)
		publication := publishGitSupplement(t, fixture)
		guard := &noWriteHistoryPG{gitContributionApplyPGStore: fixture.storage}
		fixture.scans.core.meta = guard
		prepared := prepareGitSupplementClaim(t, fixture, storage, publication)
		authorization, err := authorizeGitAdmission(systemTestContext(), fixture.basis.worker, fixture.basis.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		before := fixture.state(t, systemTestContext())
		guard.readCalls.Store(0)
		if err := fixture.scans.applyGitContributionSupplement(systemTestContext(), prepared, authorization); err != nil {
			t.Fatal(err)
		}
		if guard.readCalls.Load() != 0 || guard.writeCalls.Load() != 0 {
			t.Fatal("memory application rebuilt whole history")
		}
		assertGitSupplementEffect(t, fixture, before, publication)
	})
}

func TestPGGitAdmissionGenerationDetectsHistoryBypassingAppRevision(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "pinned")
	fixture.observeIntegration(t, "main", true)
	prepared, authorization := fixture.prepare(t)
	ctx := systemTestContext()
	before, err := fixture.storage.RepositoryRevision(ctx, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.basis.repo.ID), Kind: "position",
		Branch: "other-source", BranchID: "other-incarnation", Source: fixture.admission.Capture, Target: fixture.admission.Capture,
		GitAfter: fixture.admission.SourceHead, CreatedAt: time.Now().UTC()}
	if err := fixture.storage.ApplyHistoryEvent(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.storage.RepositoryRevision(ctx, fixture.basis.repo.ID)
	if err != nil || before != after {
		t.Fatal("fixture must bypass app revision", before, after, err)
	}
	state := fixture.state(t, ctx)
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale proof survived hidden history change", err)
	}
	if !reflect.DeepEqual(state, fixture.state(t, ctx)) {
		t.Fatal("stale work changed effects")
	}
	if _, err := fixture.scans.prepareGitContribution(ctx, fixture.admission, fixture.head); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("fresh preparation ignored competing owner", err)
	}
}

func TestPGGitAdmissionGenerationDefersChangedProviderReadWithoutLosingPage(t *testing.T) {
	scans, fixture, storage, reader, _ := admissionBatchReconcileFixture(t, 1)
	ctx := systemTestContext()
	reader.duringRead = func(_ context.Context) {
		reader.duringRead = nil
		event := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.repo.ID), Kind: "position", Branch: "other-source", BranchID: "other-incarnation",
			Source: fixture.frozen.input.Capture, Target: fixture.frozen.input.Capture, GitAfter: fixture.frozen.input.SourceHead, CreatedAt: time.Now().UTC()}
		if err := storage.ApplyHistoryEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(admissions) != 0 {
		t.Fatal("new ownership admitted after provider I/O", admissions, err)
	}
	job, err := storage.GetGitHeadScan(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL)
	if err != nil || job.State != "completed" {
		t.Fatal("safe provider page lost on metadata change", job, err)
	}
	match, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || match.Version < 1 {
		t.Fatal("deferred matcher did not advance", match, err)
	}
}

func TestPGGitAdmissionGenerationNeverReusesUncommittedImage(t *testing.T) {
	fixture := newAdmissionPGFixture(t)
	ctx := systemTestContext()
	if fixture.frozen.snapshot.generation == nil {
		t.Fatal("committed freeze lacks generation")
	}
	if err := fixture.storage.WithinRepository(ctx, fixture.repo.ID, func(write context.Context) error {
		snapshot, err := fixture.worker.core.readGitAdmissionSnapshot(write, fixture.repo.ID)
		if err != nil {
			return err
		}
		if snapshot.generation != nil {
			t.Fatal("write image misidentified as committed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGGitContributionQueryReusesCoherentHistory(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	guard := &noWriteHistoryPG{gitContributionApplyPGStore: fixture.storage}
	fixture.scans.core.meta = guard
	if err := guard.WithinReadSnapshot(systemTestContext(), func(read context.Context) error {
		history, err := fixture.scans.core.ListHistory(read, fixture.basis.repo.ID)
		if err != nil {
			return err
		}
		guard.readCalls.Store(0)
		snapshot, err := fixture.scans.core.readGitAdmissionSnapshotWithHistory(read, fixture.basis.repo.ID, history)
		if err != nil {
			return err
		}
		if snapshot.generation != nil {
			t.Fatal("caller-supplied query history acquired a reusable generation")
		}
		contributions, err := fixture.scans.core.contextContributionsForEvidence(read, fixture.basis.repo.ID, history, nil)
		if err != nil {
			return err
		}
		found := false
		for _, contribution := range contributions {
			if contribution.record.ID == fixture.proof.Contribution.ID {
				found = contribution.valid
			}
		}
		if !found || guard.readCalls.Load() != 0 || guard.writeCalls.Load() != 0 {
			t.Fatal("shared projection reloaded history or lost completion")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
