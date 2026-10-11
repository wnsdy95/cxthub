//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func prepareGitSupplementClaim(t *testing.T, fixture *gitContributionQueryPGFixture, storage *supplementPGStore, publication domain.HistoryEvent) preparedGitContributionSupplement {
	t.Helper()
	ctx := systemTestContext()
	job, err := storage.ClaimGitContributionSupplement(ctx, fixture.basis.repo.ID, time.Now().UTC(), time.Minute)
	if err != nil || job.State != "processing" || job.PublicationID != publication.ID || job.ContributionID != fixture.proof.Contribution.ID || job.ProofID != fixture.proof.ID {
		t.Fatalf("exact supplement pair was not claimed: %+v %v", job, err)
	}
	prepared, err := fixture.scans.prepareGitContributionSupplement(ctx, job)
	if err != nil || prepared.job != job || prepared.publication != publication {
		t.Fatalf("claim preparation lost immutable publication: %+v %v", prepared, err)
	}
	return prepared
}

func TestPGGitContributionSupplementConcurrentPreparedClaimsSingleEffect(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	stale := prepareGitSupplementClaim(t, fixture, storage, publication)
	current := prepareGitSupplementClaim(t, fixture, storage, publication)
	if current.job.ID != stale.job.ID || current.job.Version != stale.job.Version+1 || current.job.Attempts != stale.job.Attempts+1 || !current.job.LeaseUntil.After(stale.job.LeaseUntil) || current.revision != stale.revision {
		t.Fatalf("reclaim did not fence identical prepared evidence: stale=%+v current=%+v", stale.job, current.job)
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	authorization, err := authorizeGitAdmission(ctx, fixture.basis.worker, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := fixture.state(t, ctx)
	type result struct {
		version int64
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, prepared := range []preparedGitContributionSupplement{stale, current} {
		go func() {
			<-start
			results <- result{version: prepared.job.Version, err: fixture.scans.applyGitContributionSupplement(ctx, prepared, authorization)}
		}()
	}
	close(start)
	observed := map[int64]error{}
	for range 2 {
		got := <-results
		observed[got.version] = got.err
	}
	if len(observed) != 2 || !errors.Is(observed[stale.job.Version], domain.ErrConflict) || observed[current.job.Version] != nil {
		t.Fatalf("reclaimed owner did not exclusively commit: %+v", observed)
	}
	assertGitSupplementEffect(t, fixture, before, publication)
	after := fixture.state(t, ctx)
	if err := fixture.scans.applyGitContributionSupplement(ctx, stale, authorization); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale completion replay accepted: %v", err)
	}
	if err := fixture.scans.ProcessContributions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.state(t, ctx), after) {
		t.Fatal("stale replay or automatic retry duplicated pin or evidence revision")
	}
	if _, err := storage.ClaimGitContributionSupplement(ctx, fixture.basis.repo.ID, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("completed supplement remained claimable: %v", err)
	}
}

func TestPGGitContributionSupplementPreparedRevisionAndPolicyFences(t *testing.T) {
	for _, change := range []string{"graph-revision", "evidence-revision", "revoked-binding"} {
		t.Run(change, func(t *testing.T) {
			fixture, storage := newSupplementPGFixture(t)
			publication := publishGitSupplement(t, fixture)
			prepared := prepareGitSupplementClaim(t, fixture, storage, publication)
			ctx := systemTestContext()
			authorization, err := authorizeGitAdmission(ctx, fixture.basis.worker, fixture.basis.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ErrConflict
			switch change {
			case "graph-revision", "evidence-revision":
				write := ctx
				if change == "evidence-revision" {
					write = evidenceWriteContext(write)
				}
				if err := repositoryWriteError(write, fixture.scans.core, fixture.basis.repo.ID, func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			case "revoked-binding":
				want = domain.ErrForbidden
				if err := fixture.basis.worker.id.withIdentity(ctx, func(write context.Context) error {
					connections, err := storage.ListGitHubConnections(write)
					if err != nil {
						return err
					}
					connection := connections[0]
					connection.Enabled, connection.Generation = false, connection.Generation+1
					return storage.PutGitHubConnection(write, connection)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := fixture.state(t, ctx)
			if change == "graph-revision" && before.revision.Graph == prepared.revision.Graph {
				t.Fatal("graph revision fixture did not change")
			}
			if change == "evidence-revision" && (before.revision.Evidence == prepared.revision.Evidence || before.revision.Graph != prepared.revision.Graph) {
				t.Fatal("evidence-only revision fixture did not change independently")
			}
			if err := fixture.scans.applyGitContributionSupplement(ctx, prepared, authorization); !errors.Is(err, want) {
				t.Fatalf("prepared supplement crossed %s fence: %v", change, err)
			}
			if !reflect.DeepEqual(fixture.state(t, ctx), before) {
				t.Fatal("rejected supplement changed pin, proof, refs, or revision")
			}
			if _, err := storage.FindGitContributionSupplementMemory(ctx, fixture.basis.repo.ID, fixture.proof.Contribution.ID, publication.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("rejected supplement left its immutable pin: %v", err)
			}
			if change != "revoked-binding" {
				refreshed := prepareGitSupplementClaim(t, fixture, storage, publication)
				if refreshed.job.Version <= prepared.job.Version || refreshed.revision != before.revision {
					t.Fatal("fenced claim could not reprepare current evidence")
				}
				if err := fixture.scans.applyGitContributionSupplement(ctx, refreshed, authorization); err != nil {
					t.Fatal("fresh preparation could not recover after revision drift", err)
				}
				assertGitSupplementEffect(t, fixture, before, publication)
			}
		})
	}
}

func TestPGGitContributionSupplementLatePublicationAfterSourceArchive(t *testing.T) {
	fixture, _ := newSupplementPGFixture(t)
	ctx := systemTestContext()
	archive := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(fixture.basis.repo.ID), Kind: "archive",
		Branch: fixture.admission.SourceBranch, BranchID: fixture.admission.SourceBranchID, BindingParent: fixture.admission.SourceAnchorID,
		Source: fixture.admission.Capture, Target: fixture.admission.Capture, CreatedAt: time.Now().UTC()}
	if err := fixture.scans.core.RecordHistory(ctx, archive); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.scans.core.readGitAdmissionSnapshot(ctx, fixture.basis.repo.ID)
	if err != nil || !snapshot.bindings.ByID[fixture.admission.SourceBranchID].Archived {
		t.Fatal("accepted source archive is missing", err)
	}
	lateOriginal := *fixture.gitContributionApplyPGFixture
	late := *fixture
	late.gitContributionApplyPGFixture = &lateOriginal
	late.publication.CreatedAt = archive.CreatedAt.Add(time.Second)
	publication := publishGitSupplement(t, &late)
	if !publication.CreatedAt.After(archive.CreatedAt) {
		t.Fatal("fixture did not publish a new immutable version after archive")
	}
	before := fixture.state(t, ctx)
	if len(before.proofs) != 1 || before.proofs[0] != fixture.proof {
		t.Fatal("source archive lost original integration proof")
	}
	if err := fixture.scans.ProcessContributions(ctx, 1); err != nil {
		t.Fatal("archived source blocked its historical memory supplement", err)
	}
	assertGitSupplementEffect(t, fixture, before, publication)
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "included", true)
	snapshot, err = fixture.scans.core.readGitAdmissionSnapshot(ctx, fixture.basis.repo.ID)
	if err != nil || !snapshot.bindings.ByID[fixture.admission.SourceBranchID].Archived {
		t.Fatal("historical pin reopened the archived source", err)
	}
	history, err := fixture.scans.core.ListHistory(ctx, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range history {
		if event.ID == fixture.publication.ID {
			found = true
			if event != fixture.publication {
				t.Fatal("late supplement rewrote accepted original publication")
			}
		}
	}
	if !found {
		t.Fatal("late supplement removed accepted original publication")
	}
}
