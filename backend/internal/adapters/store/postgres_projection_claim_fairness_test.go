//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func projectionClaimFairnessPG(t *testing.T, kind string) (projectionStore, context.Context, domain.ContentHash, domain.ContentHash) {
	t.Helper()
	storage, ctx, first := catalogPG(t)
	second := domain.HashContent([]byte(string(first) + " healthy repository"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: second}); err != nil {
		t.Fatal(err)
	}
	projection := storage.pendingProjection()
	if kind == "graph" {
		projection = storage.graphProjection()
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM repos WHERE id=ANY($1::text[])`, []string{string(first), string(second)}); err != nil {
			t.Error(err)
		}
	})
	for _, repo := range []domain.ContentHash{first, second} {
		if err := projection.configure(ctx, repo, true); err != nil {
			t.Fatal(err)
		}
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE `+projection.spec.work+` SET next_attempt=clock_timestamp()-interval '1 hour' WHERE repo_id=$1`, string(first))
	return projection, ctx, first, second
}

func projectionClaimWorkPG(t *testing.T, ctx context.Context, projection projectionStore, repo domain.ContentHash) projectionWork {
	t.Helper()
	work, err := scanProjectionWork(projection.pool.QueryRow(ctx, `SELECT `+projectionWorkColumns+` FROM `+projection.spec.work+` work WHERE repo_id=$1`, string(repo)))
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func projectionClaimFailurePG(t *testing.T, ctx context.Context, projection projectionStore, before projectionWork, phase, reason string, delay time.Duration) {
	t.Helper()
	after := projectionClaimWorkPG(t, ctx, projection, before.RepoID)
	if after.Dataset != before.Dataset || after.Epoch != before.Epoch || after.AppliedSequence != before.AppliedSequence ||
		after.InputSequence != before.InputSequence || after.Cursor != before.Cursor || after.pin != before.pin ||
		after.Phase != phase || after.Fence != before.Fence+1 || after.Attempts != before.Attempts+1 || !after.LeaseUntil.IsZero() {
		t.Fatalf("failure changed captured progress or lost its fence: before=%+v after=%+v", before, after)
	}
	var actualReason string
	var remaining float64
	if err := projection.pool.QueryRow(ctx, `SELECT reason,extract(epoch FROM next_attempt-clock_timestamp()) FROM `+projection.spec.work+` WHERE repo_id=$1`, string(before.RepoID)).Scan(&actualReason, &remaining); err != nil {
		t.Fatal(err)
	}
	if actualReason != reason || remaining <= delay.Seconds()-1 || remaining > delay.Seconds() {
		t.Fatalf("failure receipt: reason=%q remaining=%fs want=%q/%s", actualReason, remaining, reason, delay)
	}
}

func TestPGProjectionClaimInvalidSourceDoesNotStarvePeer(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		t.Run(kind, func(t *testing.T) {
			projection, ctx, first, second := projectionClaimFairnessPG(t, kind)
			before := projectionClaimWorkPG(t, ctx, projection, first)
			catalogExecPG(t, ctx, projection.pool, `UPDATE repository_view_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(first))
			job, err := projection.claim(ctx, time.Minute)
			if !errors.Is(err, domain.ErrIntegrity) || job != (domain.PendingProjectionLease{}) {
				t.Fatalf("invalid source must remain visible without a lease: job=%+v err=%v", job, err)
			}
			job, err = projection.claim(ctx, time.Minute)
			if err != nil || job.RepoID != second {
				t.Fatalf("bad first repository starved healthy peer: job=%+v err=%v", job, err)
			}
			projectionClaimFailurePG(t, ctx, projection, before, "needs_attention", "invalid_source", time.Second)
		})
	}
}

func TestPGProjectionClaimCatalogLockDoesNotStarvePeer(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		t.Run(kind, func(t *testing.T) {
			projection, ctx, first, second := projectionClaimFairnessPG(t, kind)
			before := projectionClaimWorkPG(t, ctx, projection, first)
			blocker := catalogTxPG(t, ctx, projection.PostgresStore)
			catalogExecPG(t, ctx, blocker, `SELECT 1 FROM repository_catalog_state WHERE repo_id=$1 FOR UPDATE`, string(first))
			claim, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			started := time.Now()
			job, err := projection.claim(claim, time.Minute)
			elapsed := time.Since(started)
			var databaseError *pgconn.PgError
			if claim.Err() != nil || elapsed >= time.Second || !errors.As(err, &databaseError) || databaseError.Code != "55P03" || job != (domain.PendingProjectionLease{}) {
				t.Fatalf("catalog lock exhausted claim budget: elapsed=%s job=%+v err=%v context=%v", elapsed, job, err, claim.Err())
			}
			t.Logf("blocked catalog claim released after %s", elapsed)
			job, err = projection.claim(ctx, time.Minute)
			if err != nil || job.RepoID != second {
				t.Fatalf("locked first repository starved healthy peer: job=%+v err=%v", job, err)
			}
			projectionClaimFailurePG(t, ctx, projection, before, "capture", "temporary_failure", time.Second)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, projection.pool, `UPDATE `+projection.spec.work+` SET next_attempt=clock_timestamp() WHERE repo_id=$1`, string(first))
			recovered, err := projection.claim(ctx, time.Minute)
			if err != nil || recovered.RepoID != first || recovered.Dataset != before.Dataset || recovered.Fence != before.Fence+2 {
				t.Fatalf("unblocked repository did not recover: job=%+v err=%v", recovered, err)
			}
		})
	}
}

func TestPGProjectionClaimFailureFencesReplacement(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		for _, replacement := range []struct{ name, update string }{
			{"new lease fence", "fence=fence+1"},
			{"new dataset", "dataset=gen_random_uuid()"},
			{"new source epoch", "source_epoch=gen_random_uuid()"},
			{"new capture progress", "cursor='new-cursor'"},
			{"new projection phase", "phase='project',input_seq=0"},
			{"new applied sequence", "applied_seq=0"},
			{"disabled", "enabled=false"},
			{"live lease", "lease_until=clock_timestamp()+interval '1 minute'"},
		} {
			t.Run(kind+"/"+replacement.name, func(t *testing.T) {
				projection, ctx, first, _ := projectionClaimFairnessPG(t, kind)
				candidate := projectionClaimWorkPG(t, ctx, projection, first)
				catalogExecPG(t, ctx, projection.pool, `UPDATE `+projection.spec.work+` SET `+replacement.update+` WHERE repo_id=$1`, string(first))
				current := projectionClaimWorkPG(t, ctx, projection, first)
				if err := projection.failClaim(ctx, candidate, domain.ErrIntegrity); !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("stale claim failure was not fenced: %v", err)
				}
				if after := projectionClaimWorkPG(t, ctx, projection, first); after != current {
					t.Fatalf("stale failure changed replacement: before=%+v after=%+v", current, after)
				}
			})
		}
	}
}

func TestPGProjectionClaimFailureBoundsPersistence(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		t.Run(kind, func(t *testing.T) {
			projection, ctx, first, _ := projectionClaimFairnessPG(t, kind)
			candidate := projectionClaimWorkPG(t, ctx, projection, first)
			blocker := catalogTxPG(t, ctx, projection.PostgresStore)
			catalogExecPG(t, ctx, blocker, `SELECT 1 FROM `+projection.spec.work+` WHERE repo_id=$1 FOR UPDATE`, string(first))
			started := time.Now()
			err := projection.failClaim(ctx, candidate, domain.ErrIntegrity)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) >= time.Second || ctx.Err() != nil {
				t.Fatalf("failure persistence was not independently bounded: elapsed=%s err=%v", time.Since(started), err)
			}
			if after := projectionClaimWorkPG(t, ctx, projection, first); after != candidate {
				t.Fatalf("timed-out failure changed work: before=%+v after=%+v", candidate, after)
			}
		})
	}
}

func TestPGProjectionClaimCancellationDoesNotPersistFailure(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		t.Run(kind, func(t *testing.T) {
			projection, ctx, first, _ := projectionClaimFairnessPG(t, kind)
			before := projectionClaimWorkPG(t, ctx, projection, first)
			blocker := catalogTxPG(t, ctx, projection.PostgresStore)
			catalogExecPG(t, ctx, blocker, `SELECT 1 FROM repository_catalog_state WHERE repo_id=$1 FOR UPDATE`, string(first))
			claim, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			job, err := projection.claim(claim, time.Minute)
			if !errors.Is(err, context.DeadlineExceeded) || job != (domain.PendingProjectionLease{}) {
				t.Fatalf("caller deadline was lost: job=%+v err=%v", job, err)
			}
			if after := projectionClaimWorkPG(t, ctx, projection, first); after != before {
				t.Fatalf("canceled claim persisted failure: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestPGProjectionClaimIdentityFailureRemainsRetryable(t *testing.T) {
	for _, kind := range []string{"pending", "graph"} {
		t.Run(kind, func(t *testing.T) {
			projection, ctx, first, second := projectionClaimFairnessPG(t, kind)
			if err := projection.RequireDocumentIdentity(ctx, first, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, projection.pool, `UPDATE `+projection.spec.work+` SET attempts=30 WHERE repo_id=$1`, string(first))
			before := projectionClaimWorkPG(t, ctx, projection, first)
			identityErr := projection.checkRepositoryDocumentIdentity(ctx, projection.pool, first, false)
			if !errors.Is(identityErr, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal("fixture did not require a compatible worker", identityErr)
			}
			if err := projection.failClaim(ctx, before, identityErr); err != nil {
				t.Fatal(err)
			}
			projectionClaimFailurePG(t, ctx, projection, before, "capture", "identity_upgrade_required", 5*time.Minute)
			job, err := projection.claim(ctx, time.Minute)
			if err != nil || job.RepoID != second {
				t.Fatalf("incompatible worker blocked healthy peer: job=%+v err=%v", job, err)
			}
			compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
			if err := projection.checkRepositoryDocumentIdentity(compatible, projection.pool, first, false); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, projection.pool, `UPDATE `+projection.spec.work+` SET next_attempt=clock_timestamp() WHERE repo_id=$1`, string(first))
			job, err = projection.claim(compatible, time.Minute)
			if err != nil || job.RepoID != first || job.Dataset != before.Dataset {
				t.Fatalf("compatible worker required manual reconfiguration: job=%+v err=%v", job, err)
			}
		})
	}
}
