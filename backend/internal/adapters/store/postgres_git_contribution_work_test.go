//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func readGitContributionWorkTestPG(t *testing.T, ctx context.Context, storage *PostgresStore, admission domain.GitContributionAdmission) domain.GitContributionWork {
	t.Helper()
	work, err := scanGitContributionWorkPG(storage.db(ctx).QueryRow(ctx, `SELECT `+gitContributionWorkColumnsPG+` FROM git_contribution_work WHERE repo_id=$1 AND admission_id=$2`, admission.Destination.RepoID, admission.ID), admission.Destination.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func writeGitContributionWorkTestPG(t *testing.T, storage *PostgresStore, work domain.GitContributionWork) {
	t.Helper()
	raw, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(context.Background(), `UPDATE git_contribution_work SET version=$3,state=$4,next_attempt=$5,lease_until=$6,payload=$7 WHERE repo_id=$1 AND admission_id=$2`, work.RepoID, work.AdmissionID, work.Version, work.State, work.NextAttempt, work.LeaseUntil, raw); err != nil {
		t.Fatal(err)
	}
}

func putGitContributionWorkAdmissionPG(t *testing.T, storage *PostgresStore, ctx context.Context, admission domain.GitContributionAdmission) {
	t.Helper()
	if err := storage.WithinRepository(ctx, admission.Destination.RepoID, func(bound context.Context) error {
		return storage.PutGitContributionAdmission(bound, admission)
	}); err != nil {
		t.Fatal(err)
	}
}

func finishGitContributionWorkTestPG(storage *PostgresStore, ctx context.Context, work domain.GitContributionWork) error {
	return storage.WithinRepository(ctx, work.RepoID, func(bound context.Context) error { return storage.FinishGitContributionWork(bound, work) })
}

func settledGitContributionWorkPG(claim domain.GitContributionWork, state string, next time.Time) domain.GitContributionWork {
	claim.State, claim.LeaseUntil, claim.NextAttempt = state, time.Time{}, next.UTC().Truncate(time.Microsecond)
	return claim
}

func advanceGitContributionHeadTestPG(t *testing.T, storage *PostgresStore, fixture gitContextHeadPGFixture, branch, identity string) gitContextHeadPGFixture {
	t.Helper()
	fixture.now = fixture.now.Add(6 * time.Minute)
	var err error
	fixture.claim, err = storage.ClaimGitHeadScan(fixture.ctx, fixture.receipt.RepoID, fixture.receipt.GitOrigin, fixture.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	observation := fixture.observations[0]
	observation.Ref = "refs/heads/" + branch
	observation = observation.WithID()
	fixture.observations = []domain.GitRefObservation{observation}
	receipt, err := domain.NewGitHeadReceipt(fixture.claim, fixture.observations, false)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	fixture.receipt = *receipt
	fixture.head = gitContextHeadForReceiptPG(*receipt, branch, identity, fixture.admission.DestinationAnchorID)
	fixture.proof.ContextHeadID = fixture.head.ID
	fixture.proof = fixture.proof.WithID()
	if err := fixture.finish(fixture.ctx, storage); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestPGGitContributionWorkEnqueueReplayAndRollback(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "cancel"}[canceled], func(t *testing.T) {
			ctx, admission, _ := gitAdmissionPGFixture(t, storage)
			repo := admission.Destination.RepoID
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			abort := errors.New("abort admission and work")
			err = storage.WithinRepository(work, repo, func(bound context.Context) error {
				if err := storage.PutGitContributionAdmission(bound, admission); err != nil {
					return err
				}
				initial := readGitContributionWorkTestPG(t, bound, storage, admission)
				if initial.Version != 0 || initial.Attempts != 0 || initial.State != "waiting" || initial.ReadCursor != admission.AfterReadVersion || initial.ReadThrough != 0 || !initial.LeaseUntil.IsZero() {
					t.Fatal("invalid initial work", initial)
				}
				var count int
				if err := peer.pool.QueryRow(ctx, `SELECT count(*) FROM git_contribution_work WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
					t.Fatal("work visible before commit", count, err)
				}
				if canceled {
					cancel()
					return work.Err()
				}
				return abort
			})
			want := abort
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if _, err := storage.GetGitContributionAdmission(ctx, repo, admission.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("admission escaped rollback", err)
			}
			if _, err := storage.ClaimGitContributionWork(ctx, repo, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("work escaped rollback", err)
			}
			putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
			initial := readGitContributionWorkTestPG(t, ctx, peer, admission)
			putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
			if got := readGitContributionWorkTestPG(t, ctx, peer, admission); got != initial {
				t.Fatal("admission replay reset queue", got, initial)
			}
			claim, err := storage.ClaimGitContributionWork(ctx, repo, time.Now().UTC().Add(time.Second), time.Minute)
			if err != nil || claim.Validate() != nil || claim.Version != 1 || claim.Attempts != 1 || claim.ReadThrough != admission.AfterReadVersion {
				t.Fatal("initial work not claimable", claim, err)
			}
			putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
			if got := readGitContributionWorkTestPG(t, ctx, peer, admission); got != claim {
				t.Fatal("replay refreshed processing claim", got)
			}
			if after, err := peer.RepositoryRevision(ctx, repo); err != nil || after != before {
				t.Fatal("work advanced public revision", before, after, err)
			}
		})
	}
}

func TestPGGitContributionWorkEnqueueFailureRollsBackAdmissionSavepoint(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	err := storage.WithinRepository(ctx, admission.Destination.RepoID, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `ALTER TABLE git_contribution_work ADD CONSTRAINT reject_enqueue CHECK (false) NOT VALID`); err != nil {
			return err
		}
		if err := storage.PutGitContributionAdmission(bound, admission); err == nil {
			t.Fatal("enqueue failure ignored")
		}
		if _, err := storage.GetGitContributionAdmission(bound, admission.Destination.RepoID, admission.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("failed enqueue left admission or poisoned transaction", err)
		}
		if _, err := storage.db(bound).Exec(bound, `ALTER TABLE git_contribution_work DROP CONSTRAINT reject_enqueue`); err != nil {
			return err
		}
		return storage.PutGitContributionAdmission(bound, admission)
	})
	if err != nil {
		t.Fatal(err)
	}
	readGitContributionWorkTestPG(t, ctx, storage, admission)
}

func TestPGGitContributionWorkFrozenSweepAndMonotonicCursor(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	now := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	claim, err := storage.ClaimGitContributionWork(ctx, repo, now, time.Hour)
	if err != nil || claim.ReadThrough != fixture.admission.AfterReadVersion {
		t.Fatal("empty sweep invented heads", claim, err)
	}
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	reset := settledGitContributionWorkPG(claim, "waiting", now)
	reset.ReadCursor, reset.ReadThrough = reset.AfterReadVersion, 0
	if err := finishGitContributionWorkTestPG(storage, ctx, reset); err != nil {
		t.Fatal("drained empty sweep did not reset", err)
	}
	wrongBranch := advanceGitContributionHeadTestPG(t, storage, fixture, "other", "other-identity")
	claim, err = storage.ClaimGitContributionWork(ctx, repo, now, time.Hour)
	if err != nil || claim.ReadThrough != fixture.receipt.Version {
		t.Fatal("scope filter admitted other destination", claim, err)
	}
	later := advanceGitContributionHeadTestPG(t, storage, wrongBranch, "main", fixture.admission.Destination.DestinationBranchID)
	next := settledGitContributionWorkPG(claim, "waiting", now)
	for _, candidate := range []domain.GitContributionWork{
		func() domain.GitContributionWork {
			changed := next
			changed.ReadThrough = later.receipt.Version
			return changed
		}(),
		func() domain.GitContributionWork { changed := next; changed.ReadThrough = 0; return changed }(),
		func() domain.GitContributionWork { changed := next; changed.Attempts++; return changed }(),
		func() domain.GitContributionWork { changed := next; changed.Version++; return changed }(),
		func() domain.GitContributionWork {
			changed := next
			changed.AfterReadVersion++
			changed.ReadCursor++
			return changed
		}(),
	} {
		if err := finishGitContributionWorkTestPG(storage, ctx, candidate); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("claim/cursor fence bypassed", candidate, err)
		}
	}
	next.ReadCursor = claim.ReadThrough
	if err := finishGitContributionWorkTestPG(storage, ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := finishGitContributionWorkTestPG(storage, ctx, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("finished claim replay accepted", err)
	}
	claim, err = storage.ClaimGitContributionWork(ctx, repo, now, time.Hour)
	if err != nil || claim.ReadThrough != fixture.receipt.Version || claim.ReadCursor != fixture.receipt.Version {
		t.Fatal("existing sweep widened", claim, err)
	}
	next = settledGitContributionWorkPG(claim, "waiting", now)
	next.ReadCursor--
	if err := finishGitContributionWorkTestPG(storage, ctx, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("cursor backtracked inside frozen sweep", err)
	}
	next.ReadCursor, next.ReadThrough = claim.AfterReadVersion, 0
	if err := finishGitContributionWorkTestPG(storage, ctx, next); err != nil {
		t.Fatal(err)
	}
	claim, err = peer.ClaimGitContributionWork(ctx, repo, now, time.Hour)
	if err != nil || claim.ReadThrough != later.receipt.Version || claim.ReadCursor != claim.AfterReadVersion {
		t.Fatal("next sweep failed to revisit late heads", claim, err)
	}
}

func TestPGGitContributionWorkLeaseExpiryAndConcurrentClaim(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
	repo := admission.Destination.RepoID
	initial := readGitContributionWorkTestPG(t, ctx, storage, admission)
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	initial.NextAttempt = past
	writeGitContributionWorkTestPG(t, storage, initial)
	expired, err := storage.ClaimGitContributionWork(ctx, repo, past, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishGitContributionWorkTestPG(storage, ctx, settledGitContributionWorkPG(expired, "waiting", past)); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired lease finished without reclaim", err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		claim domain.GitContributionWork
		err   error
	}
	ready, release, results := make(chan struct{}, 2), make(chan struct{}), make(chan result, 2)
	now := time.Now().UTC().Add(time.Second)
	for _, writer := range []*PostgresStore{storage, peer} {
		go func() {
			ready <- struct{}{}
			<-release
			claim, err := writer.ClaimGitContributionWork(bounded, repo, now, time.Minute)
			results <- result{claim, err}
		}()
	}
	<-ready
	<-ready
	close(release)
	winners := 0
	for range 2 {
		select {
		case got := <-results:
			if got.err == nil {
				winners++
				if got.claim.Version != expired.Version+1 || got.claim.Attempts != expired.Attempts+1 || got.claim.ReadThrough != expired.ReadThrough {
					t.Fatal("reclaim lost generation or frozen sweep", got.claim)
				}
			} else if !errors.Is(got.err, domain.ErrNotFound) {
				t.Fatal(got.err)
			}
		case <-bounded.Done():
			t.Fatal(bounded.Err())
		}
	}
	if winners != 1 {
		t.Fatal("multiple concurrent claim owners", winners)
	}
	if err := finishGitContributionWorkTestPG(storage, ctx, settledGitContributionWorkPG(expired, "waiting", now)); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale worker finished replacement claim", err)
	}
}

func TestPGGitContributionWorkFairRotationAndTerminalStates(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	repo := admission.Destination.RepoID
	now := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	ids := []string{}
	for _, publication := range []string{"3", "6", "7"} {
		candidate := admission
		candidate.PublicationID = strings.Repeat(publication, 32)
		candidate = candidate.WithID()
		putGitContributionWorkAdmissionPG(t, storage, ctx, candidate)
		initial := readGitContributionWorkTestPG(t, ctx, storage, candidate)
		initial.NextAttempt = now
		writeGitContributionWorkTestPG(t, storage, initial)
		ids = append(ids, candidate.ID)
	}
	sort.Strings(ids)
	for index, id := range ids {
		claim, err := storage.ClaimGitContributionWork(ctx, repo, now, time.Minute)
		if err != nil || claim.AdmissionID != id {
			t.Fatal("queue order not deterministic", claim, id, err)
		}
		next := settledGitContributionWorkPG(claim, "waiting", now.Add(time.Minute))
		if index == 1 {
			next.State, next.Reason = "attention", "integrity_check_failed"
		}
		if index == 2 {
			next.State = "retrying"
		}
		if err := finishGitContributionWorkTestPG(storage, ctx, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.ClaimGitContributionWork(ctx, repo, now, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("not-due/attention row claimed", err)
	}
	for _, id := range []string{ids[0], ids[2]} {
		claim, err := storage.ClaimGitContributionWork(ctx, repo, now.Add(time.Minute), time.Minute)
		if err != nil || claim.AdmissionID != id {
			t.Fatal("fair retry did not rotate", claim, id, err)
		}
	}
}

func TestPGGitContributionWorkProofFinishAtomicRollback(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimGitContributionWork(ctx, repo, time.Now().UTC().Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	complete := settledGitContributionWorkPG(claim, "completed", claim.NextAttempt)
	complete.ReadCursor = fixture.receipt.Version
	if err := finishGitContributionWorkTestPG(storage, ctx, complete); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("completion without proof accepted", err)
	}
	apply := func(bound context.Context) error {
		if err := storage.PutGitContribution(bound, fixture.proof.Contribution); err != nil {
			return err
		}
		if err := storage.PutGitContributionProof(bound, fixture.proof); err != nil {
			return err
		}
		return storage.FinishGitContributionWork(bound, complete)
	}
	abort := errors.New("abort proof and completed work")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := apply(bound); err != nil {
			return err
		}
		if got := readGitContributionWorkTestPG(t, bound, storage, fixture.admission); got != complete {
			t.Fatal("transaction lost completed work", got)
		}
		if got := readGitContributionWorkTestPG(t, ctx, peer, fixture.admission); got != claim {
			t.Fatal("uncommitted completion visible", got)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if got := readGitContributionWorkTestPG(t, ctx, peer, fixture.admission); got != claim {
		t.Fatal("work escaped outer rollback", got)
	}
	if _, err := peer.FindGitContributionProof(ctx, repo, fixture.admission.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof escaped rollback", err)
	}
	if rows, err := peer.ListGitContributions(ctx, repo); err != nil || len(rows) != 0 {
		t.Fatal("ledger escaped rollback", rows, err)
	}
	if err := storage.WithinRepository(ctx, repo, apply); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimGitContributionWork(ctx, repo, time.Now().UTC().Add(time.Hour), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("completed work reclaimed", err)
	}
	if after, err := peer.RepositoryRevision(ctx, repo); err != nil || before != after {
		t.Fatal("storage work advanced revision", before, after, err)
	}
}

func TestPGGitContributionWorkTransactionPolicyAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
	repo := admission.Destination.RepoID
	now := time.Now().UTC().Add(time.Second)
	claim, err := storage.ClaimGitContributionWork(ctx, repo, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next := settledGitContributionWorkPG(claim, "waiting", now)
	if err := storage.FinishGitContributionWork(ctx, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("finish outside transaction accepted", err)
	}
	check := func(bound context.Context, writer *PostgresStore, want error) {
		t.Helper()
		if _, err := writer.ClaimGitContributionWork(bound, repo, now, time.Minute); !errors.Is(err, want) {
			t.Fatal("claim transaction/policy bypassed", err, want)
		}
		if err := writer.FinishGitContributionWork(bound, next); !errors.Is(err, want) {
			t.Fatal("finish transaction/policy bypassed", err, want)
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error { check(bound, storage, domain.ErrConflict); return nil }); err != nil {
		t.Fatal(err)
	}
	other := domain.HashContent([]byte("other work scope"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, other, func(bound context.Context) error { check(bound, storage, domain.ErrConflict); return nil }); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		escaped = bound
		check(bound, peer, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check(escaped, storage, domain.ErrConflict)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	check(canceled, storage, context.Canceled)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		check(bound, storage, domain.ErrDocumentIdentityUpgradeRequired)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	if err := finishGitContributionWorkTestPG(storage, compatible, next); err != nil {
		t.Fatal("compatible worker could not finish", err)
	}
	initial := readGitContributionWorkTestPG(t, ctx, storage, admission)
	abort := errors.New("abort claimed work")
	if err := storage.WithinRepository(compatible, repo, func(bound context.Context) error {
		if _, err := storage.ClaimGitContributionWork(bound, repo, now, time.Minute); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if got := readGitContributionWorkTestPG(t, ctx, storage, admission); got != initial {
		t.Fatal("claim escaped caller transaction", got)
	}
}

func TestPGGitContributionWorkValidationOverflowAndTampering(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, admission, _ := gitAdmissionPGFixture(t, storage)
	putGitContributionWorkAdmissionPG(t, storage, ctx, admission)
	repo := admission.Destination.RepoID
	now := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	for _, input := range []struct {
		repo  domain.ContentHash
		now   time.Time
		lease time.Duration
	}{
		{"invalid", now, time.Minute}, {repo, time.Time{}, time.Minute}, {repo, now, 0}, {repo, now, -time.Second},
	} {
		if _, err := storage.ClaimGitContributionWork(ctx, input.repo, input.now, input.lease); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid claim accepted", err)
		}
	}
	initial := readGitContributionWorkTestPG(t, ctx, storage, admission)
	for _, scenario := range []string{"version", "attempts", "watermark", "column", "payload"} {
		changed := initial
		changed.Version, changed.Attempts, changed.NextAttempt = 1, 1, now
		want := domain.ErrIntegrity
		switch scenario {
		case "version":
			changed.Version, want = math.MaxInt64, domain.ErrConflict
		case "attempts":
			changed.Attempts, want = math.MaxInt, domain.ErrConflict
		case "watermark":
			changed.AfterReadVersion++
			changed.ReadCursor++
		}
		writeGitContributionWorkTestPG(t, storage, changed)
		if scenario == "column" {
			if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_work SET version=version+1 WHERE repo_id=$1`, repo); err != nil {
				t.Fatal(err)
			}
		}
		if scenario == "payload" {
			if _, err := storage.pool.Exec(ctx, `UPDATE git_contribution_work SET payload=jsonb_set(payload,'{repo_id}',to_jsonb($2::text)) WHERE repo_id=$1`, repo, domain.HashContent([]byte("forged repo"))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := storage.ClaimGitContributionWork(ctx, repo, now, time.Minute); !errors.Is(err, want) {
			t.Fatal("tampered/overflow claim accepted", scenario, err, want)
		}
	}
	writeGitContributionWorkTestPG(t, storage, initial)
}

func TestPGGitContributionWorkMigrationBackfillsExistingAdmissions(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	fixture := newGitContextHeadPGFixture(t, storage, false)
	ctx, repo := fixture.ctx, fixture.receipt.RepoID
	if err := fixture.finish(ctx, storage); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutGitContribution(ctx, fixture.proof.Contribution); err != nil {
		t.Fatal(err)
	}
	if err := putGitProofPG(ctx, storage, fixture.proof); err != nil {
		t.Fatal(err)
	}
	other := fixture.admission
	other.PublicationID, other.AfterReadVersion = strings.Repeat("8", 32), fixture.receipt.Version
	other = other.WithID()
	putGitContributionWorkAdmissionPG(t, storage, ctx, other)
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0088_git_contribution_work.sql")
	if err != nil {
		t.Fatal(err)
	}
	before, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback migration fixture")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `DROP TABLE git_contribution_work`); err != nil {
			return err
		}
		if _, err := storage.db(bound).Exec(bound, string(migration)); err != nil {
			return err
		}
		completed := readGitContributionWorkTestPG(t, bound, storage, fixture.admission)
		waiting := readGitContributionWorkTestPG(t, bound, storage, other)
		if completed.State != "completed" || completed.Version != 0 || completed.ReadCursor != fixture.admission.AfterReadVersion || waiting.State != "waiting" || waiting.ReadCursor != other.AfterReadVersion {
			t.Fatal("migration lost frozen original or completion", completed, waiting)
		}
		claim, err := storage.ClaimGitContributionWork(bound, repo, time.Now().UTC().Add(time.Second), time.Minute)
		if err != nil || claim.AdmissionID != other.ID || claim.Version != 1 || claim.Attempts != 1 {
			t.Fatal("backfilled work not claimable or replayed proof", claim, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if after, err := storage.RepositoryRevision(ctx, repo); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("migration advanced revision", before, after, err)
	}
}
