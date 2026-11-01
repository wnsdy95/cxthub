//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docDisplayAffinityFixturePG(test *testing.T) (docDisplayPGFixture, domain.DocDisplayTarget, map[string]domain.ContentHash) {
	test.Helper()
	isolated, _ := rootProjectionIsolatedPG(test)
	test.Setenv("CXT_TEST_DSN", isolated.pool.Config().ConnString())
	fixture := docDisplayPG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	prepared := fixture.done(test, job)
	if err := fixture.storage.CompleteDocDisplay(fixture.ctx, job, prepared.Checkpoint, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	public := fixture.targets[0]
	public.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, public, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	other := fixture.claim(test)
	if other.Target.DisplayProfile() != domain.DocDisplayProjectionProfile {
		test.Fatal("fixture claimed an unrelated profile", other.Target.DisplayProfile())
	}
	if _, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, other); err != nil {
		test.Fatal(err)
	}
	owners := make(map[string]domain.ContentHash)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		var hash domain.ContentHash
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=$1 AND profile=$2`, public.Event.Hash, profile).Scan(&hash); err != nil {
			test.Fatal(err)
		}
		owners[profile] = hash
	}
	foreign, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	seedRootPG(test, fixture.ctx, fixture.storage, foreign, fixture.doc)
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: fixture.doc.hash, RepoID: foreign, DocHash: fixture.doc.hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(context.Background(), `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, foreign); err != nil {
			test.Error(err)
		}
	})
	public.RepoID = foreign
	return fixture, public, owners
}

func displayAffinityRevisionPG(test *testing.T, fixture docDisplayPGFixture, source domain.ContentHash) int64 {
	test.Helper()
	var revision int64
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT reference_revision FROM doc_display_sources_v1 WHERE source_hash=$1`, source).Scan(&revision); err != nil {
		test.Fatal(err)
	}
	return revision
}

func TestDocDisplayPGUnboundAdmissionFencesKnownSource(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture, target, owners := docDisplayAffinityFixturePG(test)
			if profile == domain.DocDisplayStringProfile {
				target.Profile = ""
			}
			owner := owners[profile]
			before := displayAffinityRevisionPG(test, fixture, owner)
			otherProfile := domain.DocDisplayStringProfile
			if profile == otherProfile {
				otherProfile = domain.DocDisplayProjectionProfile
			}
			otherBefore := displayAffinityRevisionPG(test, fixture, owners[otherProfile])
			holder, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(holder)
			if _, err := holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, owner); err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				queued, err := fixture.storage.EnqueueDocDisplay(ctx, target, time.Now().UTC())
				if err == nil && queued.State != "waiting" {
					err = fmt.Errorf("unexpected queued state: %s", queued.State)
				}
				result <- err
			}()
			blocked := false
			for !blocked && ctx.Err() == nil {
				select {
				case err := <-result:
					test.Fatal("null-source admission bypassed the known-source fence", err)
				default:
				}
				if err := fixture.storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%INSERT INTO doc_display_jobs_v1%')`).Scan(&blocked); err != nil {
					test.Fatal(err)
				}
				if !blocked {
					time.Sleep(time.Millisecond)
				}
			}
			if !blocked {
				test.Fatal("admission never reached its source fence", ctx.Err())
			}
			var visible int
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, target.RepoID).Scan(&visible); err != nil || visible != 0 {
				test.Fatal("unfenced partial receipt became visible", visible, err)
			}
			if err := holder.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					test.Fatal(err)
				}
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if after := displayAffinityRevisionPG(test, fixture, owner); after != before+1 {
				test.Fatal("unbound admission did not advance exactly one owner revision", before, after)
			}
			if after := displayAffinityRevisionPG(test, fixture, owners[otherProfile]); after != otherBefore {
				test.Fatal("admission contaminated another profile", otherBefore, after)
			}
			var source *domain.ContentHash
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT source_hash FROM doc_display_jobs_v1 WHERE repo_id=$1 AND profile=$2`, target.RepoID, profile).Scan(&source); err != nil || source != nil {
				test.Fatal("affinity fabricated a prepared checkpoint or source binding", source, err)
			}
			if _, err := fixture.storage.EnqueueDocDisplay(ctx, target, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			if after := displayAffinityRevisionPG(test, fixture, owner); after != before+1 {
				test.Fatal("duplicate unbound admission revised an unchanged root", after)
			}
			protocol := 2
			if otherProfile == domain.DocDisplayProjectionProfile {
				protocol = 5
			}
			if _, err := fixture.storage.pool.Exec(ctx, `UPDATE doc_display_jobs_v1 SET profile=$2,worker_protocol=$3 WHERE repo_id=$1`, target.RepoID, otherProfile, protocol); err != nil {
				test.Fatal(err)
			}
			if displayAffinityRevisionPG(test, fixture, owner) != before+2 || displayAffinityRevisionPG(test, fixture, owners[otherProfile]) != otherBefore+1 {
				test.Fatal("unbound profile relocation omitted old/new source affinity")
			}
			if _, err := fixture.storage.pool.Exec(ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, target.RepoID); err != nil {
				test.Fatal(err)
			}
			if displayAffinityRevisionPG(test, fixture, owners[otherProfile]) != otherBefore+2 {
				test.Fatal("unbound root deletion omitted source affinity")
			}
		})
	}
}

func TestDocDisplayPGUnboundAdmissionQueueFailureRollsBack(test *testing.T) {
	fixture, target, owners := docDisplayAffinityFixturePG(test)
	owner := owners[target.DisplayProfile()]
	before := displayAffinityRevisionPG(test, fixture, owner)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET reference_revision=9223372036854775807 WHERE source_hash=$1`, owner); err != nil {
		test.Fatal(err)
	}
	defer func() {
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET reference_revision=$2 WHERE source_hash=$1`, owner, before); err != nil {
			test.Error(err)
		}
	}()
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err == nil {
		test.Fatal("receipt committed without a durable affinity revision")
	}
	var source *domain.ContentHash
	err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_jobs_v1 WHERE repo_id=$1`, target.RepoID).Scan(&source)
	if !errors.Is(err, pgx.ErrNoRows) || displayAffinityRevisionPG(test, fixture, owner) != 9223372036854775807 {
		test.Fatal("failed affinity revision leaked a receipt or revision", source, err)
	}
}

func TestDocDisplayPGUnboundEventOnlyRelocationRevisesBothSources(test *testing.T) {
	fixture, target, owners := docDisplayAffinityFixturePG(test)
	initial, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{DocEventIndex: fixture.targets[1].Event, Kind: domain.EventToolResult})
	if err != nil {
		test.Fatal(err)
	}
	raw, err := initial.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	nextOwner := domain.HashContent(raw)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`,
		initial.Source.EventHash, initial.Source.Profile, nextOwner, raw); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	previous := displayAffinityRevisionPG(test, fixture, owners[target.DisplayProfile()])
	next := displayAffinityRevisionPG(test, fixture, nextOwner)
	unrelated := displayAffinityRevisionPG(test, fixture, owners[domain.DocDisplayStringProfile])
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET event_hash=$2 WHERE repo_id=$1`, target.RepoID, initial.Source.EventHash); err != nil {
		test.Fatal(err)
	}
	if displayAffinityRevisionPG(test, fixture, owners[target.DisplayProfile()]) != previous+1 || displayAffinityRevisionPG(test, fixture, nextOwner) != next+1 {
		test.Fatal("event-only unbound relocation bypassed old/new source affinity")
	}
	if displayAffinityRevisionPG(test, fixture, owners[domain.DocDisplayStringProfile]) != unrelated {
		test.Fatal("event-only relocation revised another profile")
	}
	var source *domain.ContentHash
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_jobs_v1 WHERE repo_id=$1`, target.RepoID).Scan(&source); err != nil || source != nil {
		test.Fatal("event affinity fabricated prepared progress", source, err)
	}
}
