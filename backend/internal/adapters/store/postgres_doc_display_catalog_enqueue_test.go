//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docCatalogEnqueueBlockedPG(test *testing.T, fixture docDisplayPGFixture, blocker int32, query string) int32 {
	test.Helper()
	wait, cancel := context.WithTimeout(fixture.ctx, 200*time.Millisecond)
	defer cancel()
	for wait.Err() == nil {
		var writer int32
		err := fixture.storage.pool.QueryRow(wait, `SELECT pid FROM pg_stat_activity
 WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE $2 LIMIT 1`, blocker, query).Scan(&writer)
		if err == nil {
			return writer
		}
		if err != pgx.ErrNoRows {
			test.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	test.Fatal("expected actual blocked backend", blocker, query, wait.Err())
	return 0
}

func TestDocDisplayCatalogEnqueuePGAbsentJobConflict(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		for _, outcome := range []string{"commit", "rollback", "committed_locked", "changed_target", "timeout", "deleted"} {
			test.Run(profile+"/"+outcome, func(test *testing.T) {
				fixture := docDisplayPG(test)
				target := docCatalogAdmissionTargetPG(fixture, 0, profile)
				gate := docCatalogAdmissionBeginPG(test, fixture)
				winner := docCatalogAdmissionBeginPG(test, fixture)
				var gatePID, winnerPID int32
				if err := gate.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),pg_advisory_xact_lock(77813901)`).Scan(&gatePID, new(any)); err != nil {
					test.Fatal(err)
				}
				if err := winner.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.storage.pool.Exec(fixture.ctx, fmt.Sprintf(`CREATE FUNCTION test_enqueue_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF pg_backend_pid()<>%d THEN PERFORM pg_advisory_xact_lock(77813901); END IF; RETURN NEW; END $$;
 CREATE TRIGGER test_enqueue_barrier BEFORE INSERT ON doc_display_jobs_v1 FOR EACH ROW EXECUTE FUNCTION test_enqueue_barrier()`, winnerPID)); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() {
					if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER test_enqueue_barrier ON doc_display_jobs_v1; DROP FUNCTION test_enqueue_barrier()`); err != nil {
						test.Error(err)
					}
				})
				type result struct {
					job domain.DocDisplayJob
					err error
				}
				done := make(chan result, 1)
				go func() {
					job, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now())
					done <- result{job, err}
				}()
				defer func() {
					rollbackPG(gate)
					rollbackPG(winner)
					if done != nil {
						<-done
					}
				}()
				writerPID := docCatalogEnqueueBlockedPG(test, fixture, gatePID, "INSERT INTO doc_display_jobs_v1%")
				if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, winner, target, nil); err != nil {
					test.Fatal(err)
				}
				if _, err := winner.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET version=17,
 source_version=source_version+CASE WHEN $2 THEN 1 ELSE 0 END WHERE repo_id=$1`, fixture.repo, outcome == "changed_target"); err != nil {
					test.Fatal(err)
				}
				if outcome == "committed_locked" || outcome == "deleted" {
					if err := winner.Commit(fixture.ctx); err != nil {
						test.Fatal(err)
					}
					winner = docCatalogAdmissionBeginPG(test, fixture)
					if err := winner.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),version FROM doc_display_jobs_v1 WHERE repo_id=$1 FOR UPDATE`, fixture.repo).Scan(&winnerPID, new(int64)); err != nil {
						test.Fatal(err)
					}
				}
				if err := gate.Rollback(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				query := "INSERT INTO doc_display_jobs_v1%"
				if outcome == "committed_locked" || outcome == "deleted" {
					query = "SELECT %FOR UPDATE"
				}
				if blocked := docCatalogEnqueueBlockedPG(test, fixture, winnerPID, query); blocked != writerPID {
					test.Fatal("different enqueue backend", blocked, writerPID)
				}
				docCatalogAdmissionTryFencePG(test, fixture.ctx, winner, target, true)
				if outcome == "timeout" {
					observed := <-done
					done = nil
					if !errors.Is(observed.err, domain.ErrResourceOverloaded) {
						test.Fatal("unique conflict ignored bounded enqueue wait", observed)
					}
				}
				if outcome == "deleted" {
					if _, err := winner.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo); err != nil {
						test.Fatal(err)
					}
				}
				if outcome == "rollback" || outcome == "timeout" {
					if err := winner.Rollback(fixture.ctx); err != nil {
						test.Fatal(err)
					}
				} else if err := winner.Commit(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				if outcome == "timeout" {
					var count int
					if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_admissions_v1`).Scan(&count); err != nil || count != 0 {
						test.Fatal("timed-out conflict leaked admission", count, err)
					}
					return
				}
				observed := <-done
				done = nil
				if outcome == "deleted" {
					if !errors.Is(observed.err, domain.ErrConflict) {
						test.Fatal("deleted winner was overwritten", observed)
					}
					return
				}
				version := int64(17)
				if outcome == "rollback" {
					version = 0
				} else if outcome == "changed_target" {
					version++
				}
				if observed.err != nil || observed.job.Version != version || observed.job.State != "waiting" || !sameDocDisplayTarget(observed.job.Target, target) {
					test.Fatal("absent-job race changed winning target", observed)
				}
				fact := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
				if fact.source != "" || fact.revision != 1 {
					test.Fatal("duplicate or missing admission", fact)
				}
				test.Logf("observed enqueue PID %d blocked by PID %d at %s; winner still acquired affinity", writerPID, winnerPID, query)
			})
		}
	}
}

func TestDocDisplayCatalogEnqueuePGFenceBeforeSource(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		for _, mutation := range []string{"insert", "update", "delete"} {
			test.Run(profile+"/"+mutation, func(test *testing.T) {
				fixture := docDisplayPG(test)
				target := docCatalogAdmissionTargetPG(fixture, 0, profile)
				source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
				if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
					test.Fatal(err)
				}
				if mutation != "insert" {
					if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, fixture.storage.pool, target, nil); err != nil {
						test.Fatal(err)
					}
				}
				before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
				destinationProfile, destinationProtocol := domain.DocDisplayProjectionProfile, 5
				if profile == domain.DocDisplayProjectionProfile {
					destinationProfile, destinationProtocol = domain.DocDisplayStringProfile, 2
				}
				relocate := `UPDATE doc_display_jobs_v1 SET profile=$2,worker_protocol=$3 WHERE repo_id=$1`
				if mutation == "update" {
					unauthorized := docCatalogAdmissionBeginPG(test, fixture)
					_, err := unauthorized.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='completed' WHERE repo_id=$1`, fixture.repo)
					docCatalogWriterCodePG(test, err, "0A000")
					rollbackPG(unauthorized)
					permitted := docCatalogAdmissionBeginPG(test, fixture)
					docCatalogAdmissionExecPG(test, fixture.ctx, permitted, relocate, fixture.repo, destinationProfile, destinationProtocol)
					if err := permitted.Rollback(fixture.ctx); err != nil {
						test.Fatal(err)
					}
					if docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool) != before {
						test.Fatal("rejected completion or rolled-back valid relocation changed the fixture")
					}
				}
				holder := docCatalogAdmissionBeginPG(test, fixture)
				docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, target, true)
				locker := docCatalogAdmissionBeginPG(test, fixture)
				if _, err := locker.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, source.hash); err != nil {
					test.Fatal(err)
				}
				writer := docCatalogAdmissionBeginPG(test, fixture)
				var err error
				switch mutation {
				case "insert":
					_, err = docCatalogAdmissionInsertJobPG(fixture.ctx, writer, target, nil)
				case "update":
					_, err = writer.Exec(fixture.ctx, relocate, fixture.repo, destinationProfile, destinationProtocol)
				case "delete":
					_, err = writer.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo)
				}
				docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
				rollbackPG(writer)
				if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
					test.Fatal("busy admission leaked mutation")
				}
				rollbackPG(locker)
				rollbackPG(holder)
			})
		}
	}
}

func TestDocDisplayCatalogEnqueuePGUpdateUniquenessPrecedesFence(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		for _, conflict := range []string{"key", "running"} {
			for _, outcome := range []string{"commit", "rollback"} {
				test.Run(profile+"/"+conflict+"/"+outcome, func(test *testing.T) {
					fixture := docDisplayPG(test)
					original := docCatalogAdmissionTargetPG(fixture, 0, profile)
					destination := docCatalogAdmissionTargetPG(fixture, 1, profile)
					if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, fixture.storage.pool, original, nil); err != nil {
						test.Fatal(err)
					}
					winner := docCatalogAdmissionBeginPG(test, fixture)
					if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, winner, destination, nil); err != nil {
						test.Fatal(err)
					}
					if conflict == "running" {
						if _, err := winner.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=1,attempts=attempts+1,
 lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1 AND event_index=$2`, fixture.repo, destination.Event.Index); err != nil {
							test.Fatal(err)
						}
					}
					contender := docCatalogAdmissionBeginPG(test, fixture)
					var winnerPID, contenderPID int32
					if err := winner.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
						test.Fatal(err)
					}
					if err := contender.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&contenderPID); err != nil {
						test.Fatal(err)
					}
					done := make(chan error, 1)
					defer func() {
						rollbackPG(winner)
						if done != nil {
							<-done
						}
					}()
					go func() {
						statement := `UPDATE doc_display_jobs_v1 SET event_index=$3,event_hash=$4 WHERE repo_id=$1 AND event_index=$2`
						arguments := []any{fixture.repo, original.Event.Index, destination.Event.Index, destination.Event.Hash}
						if conflict == "running" {
							statement = `UPDATE doc_display_jobs_v1 SET state='running',version=1,attempts=attempts+1,
 lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1 AND event_index=$2`
							arguments = arguments[:2]
						}
						_, err := contender.Exec(fixture.ctx, statement, arguments...)
						done <- err
					}()
					if blocked := docCatalogEnqueueBlockedPG(test, fixture, winnerPID, "UPDATE doc_display_jobs_v1%"); blocked != contenderPID {
						test.Fatal("unexpected uniqueness contender", blocked, contenderPID)
					}
					docCatalogAdmissionTryFencePG(test, fixture.ctx, winner, original, true)
					docCatalogAdmissionTryFencePG(test, fixture.ctx, winner, destination, true)
					if outcome == "rollback" {
						if err := winner.Rollback(fixture.ctx); err != nil {
							test.Fatal(err)
						}
					} else if err := winner.Commit(fixture.ctx); err != nil {
						test.Fatal(err)
					}
					failure := <-done
					done = nil
					if outcome == "commit" {
						docCatalogWriterCodePG(test, failure, "23505")
					} else if failure != nil {
						test.Fatal("uniqueness rollback prevented retry", failure)
					}
					if err := contender.Rollback(fixture.ctx); err != nil {
						test.Fatal(err)
					}
					fact := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, original.Event.Hash, profile)
					if fact.revision != 1 || fact.source != "" {
						test.Fatal("failed or rolled-back relocation leaked admission", fact)
					}
				})
			}
		}
	}
}

func TestDocDisplayCatalogEnqueuePGRunningIdentityImmutable(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture := docDisplayPG(test)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
				test.Fatal(err)
			}
			fixture.claim(test)
			before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
			holder := docCatalogAdmissionBeginPG(test, fixture)
			destination := docCatalogAdmissionTargetPG(fixture, 1, profile)
			if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, holder, destination, nil); err != nil {
				test.Fatal(err)
			}
			writer := docCatalogAdmissionBeginPG(test, fixture)
			_, err := writer.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET event_index=$3
 WHERE repo_id=$1 AND event_index=$2`, fixture.repo, target.Event.Index, destination.Event.Index)
			docCatalogWriterCodePG(test, err, "0A000")
			rollbackPG(writer)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, target, true)
			rollbackPG(holder)
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("running rekey changed authoritative state")
			}
		})
	}
}

func TestDocDisplayCatalogEnqueuePGBusyMigrationRollback(test *testing.T) {
	fixture := docDisplayPG(test)
	target := fixture.targets[0]
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER doc_display_jobs_v1_a_catalog_admission_order ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_a_catalog_identity ON doc_display_jobs_v1;
 DROP FUNCTION cxthub_guard_doc_catalog_admission_order(); DROP FUNCTION cxthub_guard_doc_catalog_job_identity()`); err != nil {
		test.Fatal(err)
	}
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := holder.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET reason=reason WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0139_doc_display_catalog_enqueue.sql")
	if err != nil {
		test.Fatal(err)
	}
	migration := docCatalogAdmissionBeginPG(test, fixture)
	_, err = migration.Exec(fixture.ctx, string(raw))
	docCatalogWriterCodePG(test, err, "55P03")
	rollbackPG(migration)
	var absent bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 to_regprocedure('cxthub_guard_doc_catalog_admission_order()') IS NULL AND
 to_regprocedure('cxthub_guard_doc_catalog_job_identity()') IS NULL`).Scan(&absent); err != nil || !absent {
		test.Fatal("busy migration partially installed guards", absent, err)
	}
	if err := holder.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("failed migration rewrote immutable or admission state")
	}
	retry := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := retry.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	if err := retry.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 to_regprocedure('cxthub_guard_doc_catalog_admission_order()') IS NULL OR
 to_regprocedure('cxthub_guard_doc_catalog_job_identity()') IS NULL`).Scan(&absent); err != nil || absent {
		test.Fatal("released migration did not atomically install guards", absent, err)
	}
}

func TestDocDisplayCatalogEnqueuePGBulkMetadataPrelocksJobs(test *testing.T) {
	for _, mutation := range []string{"update", "delete"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := docDisplayPG(test)
			first := docCatalogAdmissionTargetPG(fixture, 0, domain.DocDisplayStringProfile)
			second := docCatalogAdmissionTargetPG(fixture, 1, domain.DocDisplayStringProfile)
			for _, target := range []domain.DocDisplayTarget{first, second} {
				if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, fixture.storage.pool, target, nil); err != nil {
					test.Fatal(err)
				}
			}
			holder := docCatalogAdmissionBeginPG(test, fixture)
			var holderPID int32
			if err := holder.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),version FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND event_index=$2 FOR UPDATE`, fixture.repo, second.Event.Index).Scan(&holderPID, new(int64)); err != nil {
				test.Fatal(err)
			}
			writer := docCatalogAdmissionBeginPG(test, fixture)
			if _, err := writer.Exec(fixture.ctx, `SET LOCAL enable_seqscan=off; SET LOCAL enable_bitmapscan=off`); err != nil {
				test.Fatal(err)
			}
			done := make(chan error, 1)
			defer func() {
				rollbackPG(holder)
				if done != nil {
					<-done
				}
			}()
			go func() {
				statement := `UPDATE doc_display_jobs_v1 SET profile='public-event-display-v1',worker_protocol=5 WHERE repo_id=$1 AND profile=$2`
				if mutation == "delete" {
					statement = `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2`
				}
				_, err := writer.Exec(fixture.ctx, statement, fixture.repo, first.DisplayProfile())
				done <- err
			}()
			docCatalogEnqueueBlockedPG(test, fixture, holderPID, "%doc_display_jobs_v1%")
			witness := docCatalogAdmissionBeginPG(test, fixture)
			_, err := witness.Exec(fixture.ctx, `SELECT version FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND event_index=$2 FOR UPDATE NOWAIT`, fixture.repo, first.Event.Index)
			docCatalogWriterCodePG(test, err, "55P03")
			rollbackPG(witness)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, first, true)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, second, true)
			if err := holder.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			failure := <-done
			done = nil
			if failure != nil {
				test.Fatal("bulk metadata did not finish after all job locks", failure)
			}
			if err := writer.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
		})
	}
}
