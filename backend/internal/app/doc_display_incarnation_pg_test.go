//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayIncarnationProbePG struct {
	*store.PostgresStore
	pool          *pgxpool.Pool
	changed       bool
	catalogEpoch  bool
	database      string
	before, after string
}

func (probe *docDisplayIncarnationProbePG) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	if !probe.changed {
		use, sealed := step.CatalogUse()
		if !sealed {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		transaction, err := probe.pool.Begin(ctx)
		if err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = transaction.Rollback(cleanup)
		}()
		var raw []byte
		if err := transaction.QueryRow(ctx, `SELECT source.bytes,admission.generation::text FROM doc_display_sources_v1 source
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash AND admission.profile=source.profile
 AND admission.source_hash=source.source_hash WHERE source.source_hash=$1`, use.Source).Scan(&raw, &probe.before); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		if domain.HashContent(raw) != use.Source || probe.before != use.Generation {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		if probe.catalogEpoch {
			var owned bool
			if err := transaction.QueryRow(ctx, `SELECT current_database()=$1`, probe.database).Scan(&owned); err != nil || !owned || probe.database == "" {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`,
				job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if err := transaction.QueryRow(ctx, `SELECT catalog_epoch::text FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$1`, use.Source).Scan(&probe.before); err != nil || probe.before != use.Epoch {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			var guards string
			if err := transaction.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_catalog_lifecycle_v1'::regclass AND tgenabled='O'`).Scan(&guards); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 DISABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if err := transaction.QueryRow(ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET catalog_epoch=gen_random_uuid()
 WHERE source_hash=$1 RETURNING catalog_epoch::text`, use.Source).Scan(&probe.after); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 ENABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			var restored, generation string
			if err := transaction.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_catalog_lifecycle_v1'::regclass`).Scan(&restored); err != nil || restored != guards {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			if err := transaction.QueryRow(ctx, `SELECT generation::text FROM doc_display_catalog_admissions_v1 WHERE source_hash=$1`, use.Source).Scan(&generation); err != nil || generation != use.Generation {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
		} else {
			for _, replacement := range [][]byte{[]byte(`{}`), raw} {
				if tag, err := transaction.Exec(ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, use.Source, replacement); err != nil || tag.RowsAffected() != 1 {
					return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
				}
			}
			if err := transaction.QueryRow(ctx, `SELECT generation::text FROM doc_display_catalog_admissions_v1 WHERE source_hash=$1`, use.Source).Scan(&probe.after); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
		}
		if probe.after == probe.before {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		probe.changed = true
	}
	return probe.PostgresStore.SaveDocEventDisplay(ctx, job, step)
}

func TestPGDocDisplayIncarnationChangeRetriesAutomatically(test *testing.T) {
	for _, epoch := range []bool{false, true} {
		name := "source_incarnation"
		if epoch {
			name = "catalog_epoch"
		}
		test.Run(name, func(test *testing.T) {
			runDocDisplayIncarnationRecoveryPG(test, epoch)
		})
	}
}

func runDocDisplayIncarnationRecoveryPG(test *testing.T, epoch bool) {
	test.Helper()
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
	fixture := newDocDisplayPGFixture(test)
	probe := &docDisplayIncarnationProbePG{PostgresStore: fixture.storage, pool: fixture.pool, catalogEpoch: epoch, database: fixture.pool.Config().ConnConfig.Database}
	fixture.service.ConfigureDocumentDisplay(probe)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{
		Kind: domain.EventToolResult, CallID: test.Name(), Output: map[string]any{"result": "recovered", "valid": true},
	}}}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	target := docDisplayTargetPG(test, fixture, hash, 0)
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrDocDisplayIncarnationChanged) || !errors.Is(err, domain.ErrConflict) {
		test.Fatal("actual worker did not report the stale calculated incarnation", err)
	}
	if !probe.changed || probe.before == probe.after {
		test.Fatal("actual Source ABA was not committed")
	}
	var state, reason string
	var attempts int
	var released bool
	var delay int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,reason,attempts,lease_until IS NULL,
 GREATEST(ceil(extract(epoch FROM next_attempt-clock_timestamp())*1000000000),0)::bigint
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
		target.RepoID, target.DocHash, target.Event.Index, target.DisplayProfile()).Scan(&state, &reason, &attempts, &released, &delay); err != nil {
		test.Fatal(err)
	}
	if state != "retrying" || reason != "source_incarnation_changed" || attempts != 1 || !released || delay < 0 || delay > int64(3*time.Second) {
		test.Fatal("incarnation change became permanent rejection or bypassed bounded backoff", state, reason, attempts, released, time.Duration(delay))
	}
	if _, err := readDocEventDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("failed incarnation published a partial generation", err)
	}
	worker, stop := context.WithTimeout(fixture.ctx, 10*time.Second)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		fixture.service.RunDocDisplayWorker(worker)
	}()
	defer func() {
		stop()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			test.Error("owned background worker did not stop after cancellation")
		}
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		generation, err := repositoryReadForRepo(worker, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayProjectionGeneration, error) {
			return fixture.storage.ReadDocEventDisplayGeneration(read, target)
		})
		if err == nil {
			if generation.Validate() != nil || generation.Pages.Count == 0 || generation.Source.TextBytes == 0 {
				test.Fatal("automatic recovery published an invalid generation")
			}
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state,reason,attempts FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
				target.RepoID, target.DocHash, target.Event.Index, target.DisplayProfile()).Scan(&state, &reason, &attempts); err != nil || state != "completed" || reason != "" || attempts < 2 {
				test.Fatal("recovered generation lost its successful receipt", state, reason, attempts, err)
			}
			return
		}
		if !errors.Is(err, domain.ErrNotFound) {
			test.Fatal(err)
		}
		select {
		case <-worker.Done():
			test.Fatal("background retry never completed without administrative data repair", worker.Err())
		case <-ticker.C:
		}
	}
}
