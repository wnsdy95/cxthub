//go:build postgres

package store

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocCatalogAdmissionPGExistingSourceWaitFollowsFence(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture, target, owners := docDisplayAffinityFixturePG(test)
			target.Profile = profile
			holder, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(holder)
			var holderID int
			if err := holder.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&holderID); err != nil {
				test.Fatal(err)
			}
			if _, err := holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, owners[profile]); err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			defer func() {
				rollbackPG(holder)
				cancel()
				if result != nil {
					<-result
				}
			}()
			go func() {
				_, err := fixture.storage.EnqueueDocDisplay(ctx, target, time.Now().UTC())
				result <- err
			}()
			observed := false
			until := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(until) {
				if err := fixture.storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity activity
 WHERE activity.pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(activity.pid)))`, holderID).Scan(&observed); err != nil {
					test.Fatal(err)
				}
				if observed {
					break
				}
				select {
				case err := <-result:
					result = nil
					test.Fatal("admission did not reach its existing source-lock wait", err)
				default:
				}
				time.Sleep(time.Millisecond)
			}
			if !observed {
				test.Fatal("source blocker was not observed within the enqueue budget")
			}
			probe, err := fixture.storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(probe)
			var available bool
			if err := probe.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`,
				"cxthub-doc-catalog-affinity:"+string(target.Event.Hash)+":"+profile).Scan(&available); err != nil || available {
				test.Fatal("source-lock waiter did not first hold its admission fence", available, err)
			}
			if err := probe.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			failure := <-result
			result = nil
			if failure != nil {
				test.Fatal("admission failed after its real source blocker released", failure)
			}
		})
	}
}

func TestDocCatalogAdmissionPGUpsertConflictPrecedesFence(test *testing.T) {
	fixture, target, _ := docDisplayAffinityFixturePG(test)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	var holderID int
	if err := holder.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&holderID); err != nil {
		test.Fatal(err)
	}
	if _, err := holder.Exec(fixture.ctx, `SELECT repo_id FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4 FOR UPDATE`,
		target.RepoID, target.DocHash, target.Event.Index, target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	contender := docCatalogAdmissionBeginPG(test, fixture)
	var contenderID int
	if err := contender.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&contenderID); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := contender.Exec(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,created_at,next_attempt,worker_protocol)
 SELECT repo_id,doc_hash,profile,event_index,$5,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,created_at,next_attempt,worker_protocol
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4
 ON CONFLICT(repo_id,doc_hash,event_index,profile) DO UPDATE SET event_hash=EXCLUDED.event_hash`,
			target.RepoID, target.DocHash, target.Event.Index, target.DisplayProfile(), fixture.targets[1].Event.Hash)
		result <- err
	}()
	observed := false
	until := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(until) {
		if err := fixture.storage.pool.QueryRow(ctx, `SELECT $1::integer=ANY(pg_blocking_pids($2))`, holderID, contenderID).Scan(&observed); err != nil {
			test.Fatal(err)
		}
		if observed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !observed {
		test.Fatal("UPSERT did not observe the already-owned job lock")
	}
	probe := docCatalogAdmissionBeginPG(test, fixture)
	docCatalogAdmissionTryFencePG(test, ctx, probe, target, true)
	other := target
	other.Event.Hash = fixture.targets[1].Event.Hash
	docCatalogAdmissionTryFencePG(test, ctx, probe, other, true)
	rollbackPG(probe)
	if err := holder.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-result; err != nil {
		test.Fatal("UPSERT could not resume after job-lock release", err)
	}
	if err := contender.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("rolled-back legacy UPSERT leaked target or admission mutations")
	}
}
