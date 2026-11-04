//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var docCatalogWriterTablesPG = []string{"doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_key_sources_v1", "doc_tool_key_observations_v1", "doc_tool_key_members_v1", "doc_tool_order_sources_v1", "doc_tool_order_plans_v1"}

func docCatalogWriterCodePG(test *testing.T, err error, expected string) {
	test.Helper()
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != expected {
		test.Fatal("writer fence SQLSTATE", err, expected)
	}
}

func docCatalogWriterBindingPG(test *testing.T, job domain.DocDisplayJob) string {
	test.Helper()
	raw, err := json.Marshal(map[string]any{"repo": job.RepoID, "doc": job.DocHash, "event": job.Target.Event.Index, "profile": job.Target.DisplayProfile(), "version": job.Version})
	if err != nil {
		test.Fatal(err)
	}
	return string(raw)
}

func claimDocCatalogWriterProtocolOnePG(test *testing.T, fixture docDisplayPGFixture, prior domain.DocDisplayJob) domain.DocDisplayJob {
	return claimDocCatalogHistoricalProtocolPG(test, fixture, prior, "1")
}

func claimDocCatalogHistoricalProtocolPG(test *testing.T, fixture docDisplayPGFixture, prior domain.DocDisplayJob, protocol string) domain.DocDisplayJob {
	test.Helper()
	if protocol != "1" && protocol != "2" {
		test.Fatal("unsupported historical catalog protocol", protocol)
	}
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_display_protocol','5',true),set_config('cxthub.doc_catalog_protocol',$1,true)`, protocol); err != nil {
		test.Fatal(err)
	}
	job, err := scanDocDisplayJob(transaction.QueryRow(fixture.ctx, `UPDATE doc_display_jobs_v1
 SET state='running',reason='',attempts=attempts+1,version=version+1,lease_until=clock_timestamp()+interval '1 minute'
 WHERE `+docDisplayKey(prior.Target)+` AND state='waiting' AND version=$4 AND lease_until IS NULL RETURNING `+docDisplayJobColumns,
		prior.RepoID, prior.DocHash, prior.Target.Event.Index, prior.Version+1))
	if err != nil || !sameDocDisplayTarget(job.Target, prior.Target) || job.Version != prior.Version+2 {
		test.Fatal("historical catalog claim", protocol, job, err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	return job
}

func renewDocCatalogWriterProtocolOnePG(fixture docDisplayPGFixture, job domain.DocDisplayJob) error {
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_writer($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
		return err
	}
	var before, after string
	if err := transaction.QueryRow(fixture.ctx, `SELECT (to_jsonb(job)-'lease_until')::text
 FROM doc_display_jobs_v1 job WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>clock_timestamp()`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Version).Scan(&before); err != nil {
		return err
	}
	requested := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)
	var actual time.Time
	if err := transaction.QueryRow(fixture.ctx, `UPDATE doc_display_jobs_v1 SET lease_until=GREATEST(lease_until,$5)
 WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>clock_timestamp()
 RETURNING (to_jsonb(doc_display_jobs_v1)-'lease_until')::text,lease_until`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, requested).Scan(&after, &actual); err != nil {
		return err
	}
	if after != before || !actual.Equal(requested) {
		return fmt.Errorf("historical protocol-1 renewal changed metadata or lease")
	}
	return transaction.Commit(fixture.ctx)
}

func TestDocDisplayCatalogWriterPGMigrationDrainsAndPreserves(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			isolated, _ := rootProjectionIsolatedPG(test)
			test.Setenv("CXT_TEST_DSN", isolated.pool.Config().ConnString())
			fixture := docDisplayPG(test)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
				test.Fatal(err)
			}
			job := fixture.claim(test)
			var publicStep domain.DocEventDisplayStep
			var stringStep domain.DocDisplayStringStep
			var checkpoint domain.ContentHash
			if profile == domain.DocDisplayProjectionProfile {
				prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				publicStep = docEventDisplayBoundaryStep(test, fixture, job, prepared)
			} else {
				prepared := fixture.initialize(test, job)
				checkpoint, stringStep = prepared.Checkpoint, fixture.step(test, prepared)
			}
			var before []byte
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT coalesce(preparation,checkpoint) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&before); err != nil {
				test.Fatal(err)
			}
			transaction, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			for _, table := range docCatalogWriterTablesPG {
				if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER `+pgx.Identifier{table + "_a_catalog_writer"}.Sanitize()+` ON `+pgx.Identifier{table}.Sanitize()); err != nil {
					test.Fatal(err)
				}
			}
			if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER guard_doc_display_z_catalog_claim ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_z_catalog_renew ON doc_display_jobs_v1;
 DROP TRIGGER guard_doc_display_zz_catalog_write ON doc_display_jobs_v1;
 DROP FUNCTION cxthub_guard_doc_catalog_claim(); DROP FUNCTION cxthub_guard_doc_catalog_job_write();
 DROP FUNCTION cxthub_guard_doc_tool_catalog_write(); DROP FUNCTION cxthub_require_doc_catalog_writer(text,boolean,jsonb);
 DROP FUNCTION cxthub_bind_doc_catalog_writer(text,text,integer,text,bigint)`); err != nil {
				test.Fatal(err)
			}
			raw, err := os.ReadFile("../../../../schemas/db/migrations/0138_doc_display_catalog_writer.sql")
			if err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
				test.Fatal(err)
			}
			if err := transaction.Commit(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			var state string
			var version int64
			var after []byte
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,version,coalesce(preparation,checkpoint) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&state, &version, &after); err != nil || state != "waiting" || version != job.Version+1 || string(after) != string(before) {
				test.Fatal("migration changed preparation or failed to invalidate lease", state, version, err)
			}
			legacy, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(legacy)
			if _, err := legacy.Exec(fixture.ctx, `SET LOCAL cxthub.doc_display_protocol='5'`); err != nil {
				test.Fatal(err)
			}
			_, err = legacy.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`, fixture.repo)
			docCatalogWriterCodePG(test, err, "0A000")
			rollbackPG(legacy)
			if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("old claim survived protocol migration", err)
			}
			image := fixture.image(test)
			if profile == domain.DocDisplayProjectionProfile {
				_, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, publicStep)
			} else {
				_, err = fixture.storage.SaveDocDisplayStep(fixture.ctx, job, checkpoint, stringStep)
			}
			if !errors.Is(err, domain.ErrConflict) || fixture.image(test) != image {
				test.Fatal("pre-migration calculated save was not atomically fenced", err)
			}
			current := claimDocCatalogWriterProtocolOnePG(test, fixture, job)
			if current.Version <= version {
				test.Fatal("new claim did not advance the fence", current.Version, version)
			}
			if err := renewDocCatalogWriterProtocolOnePG(fixture, current); err != nil {
				test.Fatal("new writer could not renew", err)
			}
			legacyRenew, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(legacyRenew)
			_, err = legacyRenew.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET lease_until=lease_until WHERE repo_id=$1`, fixture.repo)
			docCatalogWriterCodePG(test, err, "0A000")
		})
	}
}

func TestDocDisplayCatalogWriterPGPrivateReplayGuards(test *testing.T) {
	storage, _ := rootProjectionIsolatedPG(test)
	test.Setenv("CXT_TEST_DSN", storage.pool.Config().ConnString())
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"nested": []any{map[string]any{"key": "value"}, true}})
	prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
	before := docToolOrdersPGImage(test, fixture, job)
	for _, table := range docCatalogWriterTablesPG {
		test.Run(table, func(test *testing.T) {
			var count int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count == 0 {
				test.Fatal("private guard fixture lacks actual rows", count, err)
			}
			for _, mode := range []string{"missing", "marker_only", "bad_json", "oversized", "bad_event", "bad_version", "wrong_version"} {
				for _, statement := range []string{
					`INSERT INTO ` + pgx.Identifier{table}.Sanitize() + ` SELECT * FROM ` + pgx.Identifier{table}.Sanitize() + ` LIMIT 1 ON CONFLICT DO NOTHING`,
					`UPDATE ` + pgx.Identifier{table}.Sanitize() + ` SET scope=scope`,
				} {
					transaction, err := fixture.storage.pool.Begin(fixture.ctx)
					if err != nil {
						test.Fatal(err)
					}
					defer rollbackPG(transaction)
					if mode != "missing" {
						if _, err := transaction.Exec(fixture.ctx, `SET LOCAL cxthub.doc_catalog_protocol='3'`); err != nil {
							test.Fatal(err)
						}
					}
					expected := "0A000"
					binding := ""
					switch mode {
					case "bad_json":
						binding = "{"
					case "oversized":
						binding = strings.Repeat("x", 1025)
					case "bad_event":
						binding = strings.Replace(docCatalogWriterBindingPG(test, job), `"event":0`, `"event":"invalid"`, 1)
					case "bad_version":
						binding = strings.Replace(docCatalogWriterBindingPG(test, job), fmt.Sprintf(`"version":%d`, job.Version), `"version":"invalid"`, 1)
					}
					if binding != "" {
						if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_catalog_job',$1,true)`, binding); err != nil {
							test.Fatal(err)
						}
					}
					if mode == "wrong_version" {
						stale := job
						stale.Version++
						if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_catalog_job',$1,true)`, docCatalogWriterBindingPG(test, stale)); err != nil {
							test.Fatal(err)
						}
						expected = "55000"
					}
					_, err = transaction.Exec(fixture.ctx, statement)
					docCatalogWriterCodePG(test, err, expected)
					rollbackPG(transaction)
					if after := docToolOrdersPGImage(test, fixture, job); after != before {
						test.Fatal("fenced replay changed durable catalog", table, mode)
					}
				}
			}
		})
	}
	err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		for _, table := range docCatalogWriterTablesPG {
			columns, selection := "", "*"
			switch table {
			case "doc_tool_tree_sources_v1":
				selection = "scope,source_hash,bytes,root_from,root_to"
			case "doc_tool_key_sources_v1":
				selection = "scope,tree_scope,bytes"
			case "doc_tool_order_sources_v1":
				selection = "scope,source_hash,tree_scope,keys_scope,bytes"
			}
			if selection != "*" {
				columns = "(" + selection + ")"
			}
			if _, err := transaction.Exec(bound, `INSERT INTO `+pgx.Identifier{table}.Sanitize()+columns+` SELECT `+selection+` FROM `+pgx.Identifier{table}.Sanitize()+` LIMIT 1 ON CONFLICT DO NOTHING`); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || docToolOrdersPGImage(test, fixture, job) != before || prepared.Phase != "tool_orders_done" {
		test.Fatal("authorized exact replay changed or lost its catalog", err)
	}
}

func TestDocDisplayCatalogWriterPGExpiredReclaim(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		for _, bound := range []bool{false, true} {
			name := profile + "/unbound"
			if bound {
				name = profile + "/bound"
			}
			test.Run(name, func(test *testing.T) {
				fixture := docDisplayPG(test)
				target := docCatalogAdmissionTargetPG(fixture, 0, profile)
				if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
					test.Fatal(err)
				}
				prior := fixture.claim(test)
				if bound {
					if profile == domain.DocDisplayProjectionProfile {
						if _, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, prior); err != nil {
							test.Fatal(err)
						}
					} else {
						fixture.initialize(test, prior)
					}
				}
				var before []byte
				if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT coalesce(preparation,checkpoint) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&before); err != nil {
					test.Fatal(err)
				}
				if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, fixture.repo); err != nil {
					test.Fatal(err)
				}
				current := fixture.claim(test)
				if current.Version != prior.Version+1 || current.Attempts != prior.Attempts+1 || !current.LeaseUntil.After(time.Now()) {
					test.Fatal("expired claim was not freshly fenced", prior, current)
				}
				var after []byte
				if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT coalesce(preparation,checkpoint) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&after); err != nil || string(after) != string(before) {
					test.Fatal("reclaim lost prepared evidence", err)
				}
				if err := fixture.storage.RenewDocDisplay(fixture.ctx, prior, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("expired handle remained writable", err)
				}
			})
		}
	}
}

func TestDocDisplayCatalogWriterPGAbsentSourceFence(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture := docDisplayPG(test)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
				test.Fatal(err)
			}
			job := fixture.claim(test)
			source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
			transaction, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			if _, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
				test.Fatal(err)
			}
			creator, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(creator)
			_, err = docCatalogAdmissionInsertSourcePG(fixture.ctx, creator, source)
			docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
			rollbackPG(creator)
			rollbackPG(transaction)
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
				test.Fatal("source could not retry after writer fence released", err)
			}
		})
	}
}

func TestDocDisplayCatalogWriterPGProofStillNeedsFenceAndLease(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 2)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	scope, _ := prepared.ToolTree.Hash()
	before := docToolTreePGImage(test, fixture, job)
	holder, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(holder)
	if _, err := holder.Exec(fixture.ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, job.Target.Event.Hash, job.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	attempt := func() error {
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			return err
		}
		defer rollbackPG(transaction)
		if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_catalog_protocol','3',true),set_config('cxthub.doc_catalog_job',$1,true)`, docCatalogWriterBindingPG(test, job)); err != nil {
			return err
		}
		_, err = transaction.Exec(fixture.ctx, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=node_hash WHERE scope=$1`, scope)
		return err
	}
	docCatalogAdmissionSQLStatePG(test, attempt(), "55P03", true)
	if after := docToolTreePGImage(test, fixture, job); after != before {
		test.Fatal("manual live binding bypassed the affinity fence")
	}
	rollbackPG(holder)
	docCatalogWriterCodePG(test, attempt(), "0A000")
	if err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		_, err := transaction.Exec(bound, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=node_hash WHERE scope=$1`, scope)
		return err
	}); err != nil {
		test.Fatal("actual calculation could not acquire its rights", err)
	}
	if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, fixture.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	before = docToolTreePGImage(test, fixture, job)
	docCatalogWriterCodePG(test, attempt(), "55000")
	if after := docToolTreePGImage(test, fixture, job); after != before {
		test.Fatal("expired proof changed a private catalog")
	}
}

func TestDocDisplayCatalogWriterPGRechecksRehomedSource(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 1)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	scope, _ := prepared.ToolTree.Hash()
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	writer, err := fixture.storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(writer)
	var writerID int
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerID); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(ctx, `ALTER FUNCTION cxthub_lock_doc_catalog_affinities(text[],text[],text[]) RENAME TO cxthub_fixture_catalog_affinities`); err != nil {
		test.Fatal(err)
	}
	wrapper := fmt.Sprintf(`CREATE FUNCTION cxthub_lock_doc_catalog_affinities(native_events text[],native_profiles text[],source_hashes text[])
 RETURNS TABLE(event_hash text,profile text) LANGUAGE plpgsql AS $body$
 BEGIN
   IF pg_backend_pid()=%d THEN
     PERFORM pg_advisory_xact_lock(hashtextextended('cxthub-writer-proof-rehome',0));
   END IF;
   RETURN QUERY SELECT * FROM cxthub_fixture_catalog_affinities(native_events,native_profiles,source_hashes);
 END;
 $body$`, writerID)
	if _, err := fixture.storage.pool.Exec(ctx, wrapper); err != nil {
		test.Fatal(err)
	}
	gate, err := fixture.storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(gate)
	var gateID int
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&gateID); err != nil {
		test.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxthub-writer-proof-rehome',0))`); err != nil {
		test.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `SELECT set_config('cxthub.doc_catalog_protocol','3',true),set_config('cxthub.doc_catalog_job',$1,true)`, docCatalogWriterBindingPG(test, job)); err != nil {
		test.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := writer.Exec(ctx, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=node_hash WHERE scope=$1 AND node_from=$2`, scope, prepared.ToolTree.Root.From)
		result <- err
	}()
	observed := false
	for !observed && ctx.Err() == nil {
		if err := fixture.storage.pool.QueryRow(ctx, `SELECT $1=ANY(pg_blocking_pids($2))`, gateID, writerID).Scan(&observed); err != nil {
			test.Fatal(err)
		}
		if !observed {
			select {
			case err := <-result:
				test.Fatal("writer did not reach proof-to-fence barrier", err)
			case <-time.After(time.Millisecond):
			}
		}
	}
	if !observed {
		test.Fatal("proof-to-fence wait not observed", ctx.Err())
	}
	if _, err := fixture.storage.pool.Exec(ctx, `UPDATE doc_display_sources_v1 SET event_hash=$2 WHERE event_hash=$1 AND profile=$3`, job.Target.Event.Hash, domain.HashContent([]byte("rehomed writer source")), job.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	before := docToolTreePGImage(test, fixture, job)
	rollbackPG(gate)
	writeErr := <-result
	rollbackPG(writer)
	if _, err := fixture.storage.pool.Exec(ctx, `DROP FUNCTION cxthub_lock_doc_catalog_affinities(text[],text[],text[]);
 ALTER FUNCTION cxthub_fixture_catalog_affinities(text[],text[],text[]) RENAME TO cxthub_lock_doc_catalog_affinities`); err != nil {
		test.Fatal(err)
	}
	docCatalogProtocolDeniedPG(test, writeErr, "55000", "document catalog writer claim is not live")
	if after := docToolTreePGImage(test, fixture, job); after != before {
		test.Fatal("rehome race changed private catalog or job evidence")
	}
}

func TestDocDisplayCatalogWriterPGBusyMigrationRollback(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := rootProjectionIsolatedDSN(test, ctx)
	storage, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	directory := test.TempDir()
	entries, err := os.ReadDir("../../../../schemas/db/migrations")
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" || entry.Name() > "0137_doc_display_catalog_census.sql" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), raw, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := storage.ApplyMigrations(ctx, directory); err != nil {
		test.Fatal(err)
	}
	repo, _ := rootPGRepo(test, ctx, storage)
	seed, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(seed)
	if _, err := seed.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='5'`); err != nil {
		test.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,event_index,event_hash,profile,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,version,lease_until,worker_protocol,created_at,next_attempt)
 VALUES($1,$2,0,$3,'public-event-display-v1',0,100,0,'',gen_random_uuid(),0,$2,
 'running',7,clock_timestamp()+interval '1 minute',5,clock_timestamp(),clock_timestamp())`,
		repo, domain.HashContent([]byte("busy migration doc")), domain.HashContent([]byte("busy migration event"))); err != nil {
		test.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	holder, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(holder)
	if _, err := holder.Exec(ctx, `SELECT repo_id FROM doc_display_jobs_v1 WHERE repo_id=$1 FOR UPDATE`, repo); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0138_doc_display_catalog_writer.sql")
	if err != nil {
		test.Fatal(err)
	}
	upgrade, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(upgrade)
	_, err = upgrade.Exec(ctx, string(raw))
	docCatalogWriterCodePG(test, err, "55P03")
	rollbackPG(upgrade)
	var unchanged bool
	if err := storage.pool.QueryRow(ctx, `SELECT state='running' AND version=7 AND lease_until IS NOT NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_writer(text,text,integer,text,bigint)') IS NULL
 FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo).Scan(&unchanged); err != nil || !unchanged {
		test.Fatal("busy migration partially changed claim/schema", unchanged, err)
	}
	rollbackPG(holder)
	upgrade, err = storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(upgrade)
	if _, err := upgrade.Exec(ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	if err := upgrade.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT state='waiting' AND version=8 AND lease_until IS NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_writer(text,text,integer,text,bigint)') IS NOT NULL
 FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo).Scan(&unchanged); err != nil || !unchanged {
		test.Fatal("migration retry did not atomically drain and fence", unchanged, err)
	}
}
