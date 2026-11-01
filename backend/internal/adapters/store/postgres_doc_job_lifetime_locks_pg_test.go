//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docJobLifetimeLocksHoldPG(test *testing.T, ctx context.Context, storage *PostgresStore, claim domain.DocFinalizationJob) (pgx.Tx, uint32) {
	test.Helper()
	outer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { rollbackPG(outer) })
	inner, err := outer.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(inner)
	current, err := readLockedDocJobPG(ctx, inner, claim.RepoID, claim.ID)
	if err != nil {
		test.Fatal("retain the actual current job lifetime", err)
	}
	if current.RepositoryEpoch == "" || current.RepositoryEpoch != claim.RepositoryEpoch || !current.Fences(claim, time.Now().UTC()) {
		test.Fatal("retained job did not match its live claim", current)
	}
	if err := inner.Commit(ctx); err != nil {
		test.Fatal("release the helper savepoint without committing its outer transaction", err)
	}
	return outer, outer.Conn().PgConn().PID()
}

func docJobLifetimeLocksImagePG(test *testing.T, ctx context.Context, database pgDatabase) string {
	test.Helper()
	rows, err := database.Query(ctx, `SELECT relname FROM pg_class
 WHERE relnamespace='public'::regnamespace AND relkind='r' ORDER BY relname`)
	if err != nil {
		test.Fatal("enumerate owned-fixture durable tables", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			test.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	var image strings.Builder
	for _, name := range tables {
		var body string
		statement := `SELECT COALESCE(jsonb_agg(to_jsonb(stored) || jsonb_build_object('_xmin',stored.xmin::text)
 ORDER BY to_jsonb(stored)::text),'[]'::jsonb)::text FROM ` + pgx.Identifier{"public", name}.Sanitize() + ` stored`
		if err := database.QueryRow(ctx, statement).Scan(&body); err != nil {
			test.Fatal("snapshot owned-fixture table", name, err)
		}
		image.WriteString(name)
		image.WriteByte('\n')
		image.WriteString(body)
		image.WriteByte('\n')
	}
	return image.String()
}

func TestPGDocJobLifetimeCatalogCounterProgress(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	repo, namespace := rootPGRepo(test, ctx, storage)
	root := uniqueRootPublicationFixturePG(test, "catalog counter progress while a job lifetime is retained")
	claim, _ := rootPublicationClaimPG(test, storage, ctx, root, repo)
	holder := authorityScopePeerPG(test, storage, "", nil)
	writer := authorityScopePeerPG(test, storage, "", nil)
	outer, holderPID := docJobLifetimeLocksHoldPG(test, ctx, holder, claim)
	writerPID := authorityScopePIDPG(test, ctx, writer)
	if holderPID == writerPID || holderPID == fixture.observer.PgConn().PID() || writerPID == fixture.observer.PgConn().PID() {
		test.Fatal("holder, counter writer, and observer must use independent PostgreSQL backends")
	}
	before := rootPGImage(test, ctx, storage, repo, namespace)
	var head int64
	var epoch string
	if err := fixture.observer.QueryRow(ctx, `SELECT head_seq,epoch::text FROM repository_catalog_state WHERE repo_id=$1`, repo).Scan(&head, &epoch); err != nil {
		test.Fatal(err)
	}
	var nextHead int64
	var nextEpoch string
	if err := writer.pool.QueryRow(ctx, `SELECT seq,epoch::text FROM cxt_catalog_generation($1)`, repo).Scan(&nextHead, &nextEpoch); err != nil {
		test.Fatal("ordinary catalog allocation must commit while the helper's outer transaction is still open", err)
	}
	if nextHead != head+1 || nextEpoch != epoch || epoch != claim.RepositoryEpoch {
		test.Fatal("counter allocation changed the retained lifetime or failed to advance exactly once", head, nextHead, epoch, nextEpoch)
	}
	var committedHead int64
	var committedEpoch string
	if err := fixture.observer.QueryRow(ctx, `SELECT head_seq,epoch::text FROM repository_catalog_state WHERE repo_id=$1`, repo).Scan(&committedHead, &committedEpoch); err != nil {
		test.Fatal(err)
	}
	if committedHead != nextHead || committedEpoch != epoch {
		test.Fatal("independent observer could not see counter progress before holder commit", committedHead, committedEpoch)
	}
	if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
		test.Fatal("lifetime retention or catalog allocation changed archive, ownership, accounting, or job receipt")
	}
	if err := outer.Commit(ctx); err != nil {
		test.Fatal("commit retained lifetime after independently committed counter progress", err)
	}
	assertRootStreamingUnpublishedPG(test, storage, ctx, claim)
}

func TestPGDocJobLifetimeDeletesWaitForOuterCommit(test *testing.T) {
	for _, target := range []struct {
		name      string
		statement string
		remaining [3]int
	}{
		{"catalog-row", `DELETE FROM repository_catalog_state WHERE repo_id=$1`, [3]int{1, 0, 1}},
		{"repository-cascade", `DELETE FROM repos WHERE id=$1`, [3]int{}},
	} {
		test.Run(target.name, func(test *testing.T) {
			fixture, storage := authorityScopePG(test)
			ctx := rootPublicationContext(fixture.ctx)
			root := uniqueRootPublicationFixturePG(test, "retained lifetime deletion "+target.name)
			claim, _ := rootPublicationClaimPG(test, storage, ctx, root, "")
			holder := authorityScopePeerPG(test, storage, "", nil)
			deleter := authorityScopePeerPG(test, storage, "", nil)
			outer, holderPID := docJobLifetimeLocksHoldPG(test, ctx, holder, claim)
			deleterPID := authorityScopePIDPG(test, ctx, deleter)
			if holderPID == deleterPID || holderPID == fixture.observer.PgConn().PID() || deleterPID == fixture.observer.PgConn().PID() {
				test.Fatal("holder, deleter, and observer must use independent PostgreSQL backends")
			}
			before := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx))
			work, cancel := context.WithCancel(ctx)
			completed := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				tag, err := deleter.pool.Exec(work, target.statement, claim.RepoID)
				if err == nil && tag.RowsAffected() != 1 {
					err = fmt.Errorf("lifetime deletion affected %d rows, want one", tag.RowsAffected())
				}
				completed <- err
			}()
			defer func() {
				cancel()
				rollbackPG(outer)
				<-joined
			}()
			authorityScopeWaitPG(test, work, fixture.observer, holderPID, deleterPID, target.statement, completed)
			if after := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx)); after != before {
				test.Fatal("a blocked lifetime deletion changed durable state before outer commit")
			}
			authorityScopeWaitPG(test, work, fixture.observer, holderPID, deleterPID, target.statement, completed)
			select {
			case err := <-completed:
				test.Fatal("lifetime deletion escaped the actual helper's retained outer transaction", err)
			default:
			}
			if err := outer.Commit(ctx); err != nil {
				test.Fatal("release the actual lifetime blocker only at outer commit", err)
			}
			authorityScopeDonePG(test, work, completed)
			var remaining [3]int
			if err := fixture.observer.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM repos WHERE id=$1),
 (SELECT count(*) FROM repository_catalog_state WHERE repo_id=$1),
 (SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1)`, claim.RepoID).
				Scan(&remaining[0], &remaining[1], &remaining[2]); err != nil {
				test.Fatal(err)
			}
			if remaining != target.remaining {
				test.Fatal("lifetime deletion did not commit its expected cascade after outer commit", remaining, target.remaining)
			}
		})
	}
}

func TestPGDocJobLifetimeOfflineResetRejectsStaleCommands(test *testing.T) {
	test.Log("Privileged owned-fixture/offline-maintenance qualification only: reset commits with no concurrent command or epoch rotation; retained claims and spools represent stale pre-maintenance worker state, not a public API or supported online reset.")
	directory := rangeSpoolTempDir(test)
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	repo, namespace := rootPGRepo(test, ctx, storage)
	root := uniqueRootPublicationFixturePG(test, "committed offline catalog reset invalidates retained job authority")
	claim, proof := rootPublicationClaimPG(test, storage, ctx, root, repo)
	renewedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := storage.RenewDocJob(ctx, claim, renewedAt, 15*time.Second); err != nil {
		test.Fatal("configure a real lease through the production renewal path before maintenance", err)
	}
	claim.LeaseUntil, claim.UpdatedAt = renewedAt.Add(15*time.Second), renewedAt
	current, err := storage.GetDocJob(ctx, repo, claim.ID)
	if err != nil || current.RepositoryEpoch != claim.RepositoryEpoch || !current.LeaseUntil.Equal(claim.LeaseUntil) {
		test.Fatal("persisted pre-maintenance claim does not have the expected real lease", current, err)
	}
	streaming := prepareRootStreamingPG(test, storage, ctx, claim, directory)
	materialized, err := storage.PrepareDocJob(ctx, proof)
	if err != nil {
		test.Fatal(err)
	}
	beforeReset := rootPGImage(test, ctx, storage, repo, namespace)
	var ownsCatalog bool
	if err := storage.pool.QueryRow(ctx, `SELECT current_user=pg_get_userbyid(relowner)
 FROM pg_class WHERE oid='public.repository_catalog_state'::regclass`).Scan(&ownsCatalog); err != nil || !ownsCatalog {
		test.Fatal("offline reset must use the owned fixture's catalog owner", ownsCatalog, err)
	}
	if _, err := storage.pool.Exec(ctx, `SELECT public.cxt_reset_catalog($1)`, repo); err != nil {
		test.Fatal("commit the explicit offline operator reset before testing any stale command", err)
	}
	current, err = storage.GetDocJob(ctx, repo, claim.ID)
	if err != nil || current.RepositoryEpoch == "" || current.RepositoryEpoch == claim.RepositoryEpoch ||
		current.State != "running" || current.Version != claim.Version || !current.LeaseUntil.Equal(claim.LeaseUntil) {
		test.Fatal("committed reset did not rotate only the job's repository lifetime authority", current, err)
	}
	resetEpoch := current.RepositoryEpoch
	if after := rootPGImage(test, ctx, storage, repo, namespace); after != beforeReset {
		test.Fatal("offline catalog reset changed archive, ownership, accounting, or durable job delivery")
	}
	beforeStale := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx))
	for _, command := range []struct {
		name string
		run  func(time.Time) error
	}{
		{"renew", func(now time.Time) error { return storage.RenewDocJob(ctx, claim, now, time.Minute) }},
		{"finish-retrying", func(now time.Time) error {
			finished := claim
			finished.State, finished.Reason = "retrying", "temporary_failure"
			finished.UpdatedAt, finished.NextAttempt, finished.LeaseUntil = now, now, time.Time{}
			return storage.FinishDocJob(ctx, finished, now)
		}},
		{"finish-rejected", func(now time.Time) error {
			finished := claim
			finished.State, finished.Reason = "rejected", "invalid_document"
			finished.UpdatedAt, finished.LeaseUntil = now, time.Time{}
			return storage.FinishDocJob(ctx, finished, now)
		}},
		{"materialized-complete", func(now time.Time) error { return materialized.Complete(ctx, claim, now) }},
		{"streaming-complete", func(now time.Time) error { return streaming.Complete(ctx, claim, now) }},
	} {
		now := time.Now().UTC()
		if !claim.LeaseUntil.After(now) {
			test.Fatal("old lease expired before epoch-only rejection could be qualified", command.name)
		}
		if err := command.run(now); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("stale pre-maintenance epoch was not rejected", command.name, err)
		}
		if !claim.LeaseUntil.After(time.Now().UTC()) {
			test.Fatal("command outlived the live lease, obscuring epoch-only rejection", command.name)
		}
		if after := docJobLifetimeLocksImagePG(test, ctx, storage.db(ctx)); after != beforeStale {
			test.Fatal("stale command changed durable owned-fixture state", command.name)
		}
		assertRootStreamingUnpublishedPG(test, storage, ctx, claim)
	}
	if err := streaming.Close(); err != nil {
		test.Fatal("close the rejected pre-maintenance root spool", err)
	}
	assertRangeSpoolsRemoved(test, directory)
	expiry := time.NewTimer(time.Until(claim.LeaseUntil))
	defer expiry.Stop()
	select {
	case <-expiry.C:
	case <-ctx.Done():
		test.Fatal("fixture ended before the persisted real lease expired", ctx.Err())
	}
	fresh, err := storage.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil || fresh.ID != claim.ID || fresh.Version != claim.Version+1 || fresh.Attempts != claim.Attempts+1 ||
		fresh.RepositoryEpoch != resetEpoch || !fresh.LeaseUntil.After(time.Now().UTC()) {
		test.Fatal("fresh post-expiry claim did not bind the committed reset epoch and next version", fresh, err)
	}
	publication := prepareRootStreamingPG(test, storage, ctx, fresh, directory)
	if err := publication.Complete(ctx, fresh, time.Now().UTC()); err != nil {
		test.Fatal("fresh post-maintenance claim could not publish", err)
	}
	if err := publication.Close(); err != nil {
		test.Fatal("close the successful post-maintenance root spool", err)
	}
	assertRangeSpoolsRemoved(test, directory)
	assertRootSourceWitnessPublishedPG(test, storage, ctx, fresh, root)
	current, err = storage.GetDocJob(ctx, repo, fresh.ID)
	if err != nil || current.RepositoryEpoch != resetEpoch || current.State != "completed" || current.Version != fresh.Version {
		test.Fatal("fresh completion receipt lost the post-maintenance lifetime", current, err)
	}
}
