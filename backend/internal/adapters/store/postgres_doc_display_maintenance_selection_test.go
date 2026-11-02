//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docMaintenanceSelectionSourcePG struct {
	hash  domain.ContentHash
	event domain.ContentHash
}

type docMaintenancePickerTraceKeyPG struct{}

type docMaintenancePausedPickerPG struct {
	selected chan int32
	resume   chan struct{}
	once     sync.Once
}

func (trace *docMaintenancePausedPickerPG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT locked_cursor.after_source,work.source_hash,work.ready") {
		return context.WithValue(ctx, docMaintenancePickerTraceKeyPG{}, true)
	}
	return ctx
}

func (trace *docMaintenancePausedPickerPG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(docMaintenancePickerTraceKeyPG{}) == true {
		trace.selected <- int32(connection.PgConn().PID())
		<-trace.resume
	}
}

func (trace *docMaintenancePausedPickerPG) release() {
	trace.once.Do(func() { close(trace.resume) })
}

func docMaintenanceSelectionSourcesPG(test *testing.T, fixture docDisplayPGFixture, count int) []docMaintenanceSelectionSourcePG {
	test.Helper()
	sources := make([]docMaintenanceSelectionSourcePG, count)
	for ordinal := range sources {
		sources[ordinal].hash, sources[ordinal].event = maintenanceSource(test, fixture.ctx, fixture.storage, fmt.Sprintf("%s/%d", test.Name(), ordinal))
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, sources[ordinal].hash); err != nil {
			test.Fatal(err)
		}
	}
	sort.Slice(sources, func(first, second int) bool { return sources[first].hash < sources[second].hash })
	return sources
}

func docMaintenanceSelectionCursorPG(test *testing.T, fixture docDisplayPGFixture, expected domain.ContentHash) {
	test.Helper()
	var actual domain.ContentHash
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT after_source FROM doc_display_maintenance_cursor_v1 WHERE singleton`).Scan(&actual); err != nil || actual != expected {
		test.Fatal("durable collector position", actual, expected, err)
	}
}

func docMaintenanceSelectionReopenPG(test *testing.T, fixture docDisplayPGFixture) *PostgresStore {
	test.Helper()
	pool, err := pgxpool.New(fixture.ctx, fixture.storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}
}

func docMaintenanceSelectionGatePG(test *testing.T, fixture docDisplayPGFixture) (pgx.Tx, int32) {
	test.Helper()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `ALTER FUNCTION cxthub_lock_doc_catalog_affinities(text[],text[],text[]) RENAME TO cxthub_fixture_maintenance_affinities;
 CREATE FUNCTION cxthub_lock_doc_catalog_affinities(native_events text[],native_profiles text[],source_hashes text[])
 RETURNS TABLE(event_hash text,profile text) LANGUAGE plpgsql AS $body$
 BEGIN
   IF current_query() LIKE 'SELECT * FROM cxthub_lock_doc_catalog_affinities%' THEN
     PERFORM pg_advisory_xact_lock(77814002);
   END IF;
   RETURN QUERY SELECT * FROM cxthub_fixture_maintenance_affinities(native_events,native_profiles,source_hashes);
 END; $body$`); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP FUNCTION cxthub_lock_doc_catalog_affinities(text[],text[],text[]);
 ALTER FUNCTION cxthub_fixture_maintenance_affinities(text[],text[],text[]) RENAME TO cxthub_lock_doc_catalog_affinities`); err != nil {
			test.Error(err)
		}
	})
	gate := docCatalogAdmissionBeginPG(test, fixture)
	var gatePID int32
	if err := gate.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),pg_advisory_xact_lock(77814002)`).Scan(&gatePID, new(any)); err != nil {
		test.Fatal(err)
	}
	return gate, gatePID
}

func TestDocDisplayMaintenanceSelectionPGProgressPastBusyCap(test *testing.T) {
	for _, busy := range []string{"affinity", "source", "maintenance", "future"} {
		test.Run(busy, func(test *testing.T) {
			fixture := docDisplayPG(test)
			sources := docMaintenanceSelectionSourcesPG(test, fixture, 2*docDisplayMaintenanceCandidates+2)
			holder := docCatalogAdmissionBeginPG(test, fixture)
			for _, source := range sources[:len(sources)-1] {
				var err error
				switch busy {
				case "affinity":
					_, err = holder.Exec(fixture.ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, source.event, domain.DocDisplayProjectionProfile)
				case "source":
					_, err = holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, source.hash)
				case "maintenance":
					_, err = holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE`, source.hash)
				case "future":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp()+interval '1 day' WHERE source_hash=$1`, source.hash)
				}
				if err != nil {
					test.Fatal(err)
				}
			}
			for batch := 1; batch <= 2; batch++ {
				restarted := docMaintenanceSelectionReopenPG(test, fixture)
				if _, err := restarted.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("busy-only batch must remain bounded", busy, batch, err)
				}
				docMaintenanceSelectionCursorPG(test, fixture, sources[batch*docDisplayMaintenanceCandidates-1].hash)
				if busy == "affinity" {
					if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp()-interval '1 day'`); err != nil {
						test.Fatal(err)
					}
				}
			}
			if busy == "source" || busy == "maintenance" {
				probe := docCatalogAdmissionBeginPG(test, fixture)
				var available bool
				if err := probe.QueryRow(fixture.ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(
 'cxthub-doc-catalog-affinity:'||$1::text||':'||$2::text,0))`, sources[0].event, domain.DocDisplayProjectionProfile).Scan(&available); err != nil || !available {
					test.Fatal("skipped candidate retained affinity", available, err)
				}
				if busy == "maintenance" {
					if _, err := probe.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`, sources[0].hash); err != nil {
						test.Fatal("skipped maintenance candidate retained Source", err)
					}
				}
				rollbackPG(probe)
			}
			var census int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1`).Scan(&census); err != nil || census != 0 {
				test.Fatal("busy attempt partially wrote census", census, err)
			}
			restarted := docMaintenanceSelectionReopenPG(test, fixture)
			step, err := restarted.StepDocDisplayMaintenance(fixture.ctx)
			if err != nil || step.Source != sources[len(sources)-1].hash || step.Phase != "catalog_census" {
				test.Fatal("busy head starved unrelated source across restart", busy, step, err)
			}
			docMaintenanceSelectionCursorPG(test, fixture, step.Source)
		})
	}
}

func TestDocDisplayMaintenanceSelectionPGSmallRingAndWrap(test *testing.T) {
	fixture := docDisplayPG(test)
	sources := docMaintenanceSelectionSourcesPG(test, fixture, 3)
	for turn := 0; turn < 9; turn++ {
		step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
		expected := sources[turn%len(sources)].hash
		if err != nil || step.Source != expected {
			test.Fatal("small ring skipped an unprocessed candidate", turn, step, expected, err)
		}
		docMaintenanceSelectionCursorPG(test, fixture, expected)
	}
}

func TestDocDisplayMaintenanceSelectionPGCursorAndCancellation(test *testing.T) {
	fixture := docDisplayPG(test)
	sources := docMaintenanceSelectionSourcesPG(test, fixture, 1)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := holder.Exec(fixture.ctx, `SELECT singleton FROM doc_display_maintenance_cursor_v1 FOR UPDATE`); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("busy picker cursor blocked or leaked raw lock failure", err)
	}
	if err := holder.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	candidate, err := fixture.storage.selectDocDisplayMaintenance(fixture.ctx)
	if err != nil || candidate.source != sources[0].hash {
		test.Fatal(candidate, err)
	}
	cancelled, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if _, err := fixture.storage.StepDocDisplayMaintenance(cancelled); !errors.Is(err, context.Canceled) {
		test.Fatal("collector swallowed cancellation", err)
	}
	docMaintenanceSelectionCursorPG(test, fixture, candidate.source)
	step, err := docMaintenanceSelectionReopenPG(test, fixture).StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != candidate.source {
		test.Fatal("committed selection was permanently skipped after cancellation", step, err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, candidate.source); err != nil {
		test.Fatal(err)
	}
	docMaintenanceSelectionCursorPG(test, fixture, candidate.source)
	if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("deleted cursor target broke ring wrap", err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_maintenance_cursor_v1`); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("missing derived cursor prevented empty-queue recovery", err)
	}
	docMaintenanceSelectionCursorPG(test, fixture, "")
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_maintenance_cursor_v1`); err != nil {
		test.Fatal(err)
	}
	healthy := docMaintenanceSelectionSourcesPG(test, fixture, 1)[0]
	step, err = docMaintenanceSelectionReopenPG(test, fixture).StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != healthy.hash {
		test.Fatal("missing derived cursor required manual repair before live work", step, err)
	}
}

func TestDocDisplayMaintenanceSelectionPGBusyMigrationRollback(test *testing.T) {
	fixture := docDisplayPG(test)
	docMaintenanceSelectionSourcesPG(test, fixture, 1)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TABLE doc_display_maintenance_cursor_v1;
 DROP INDEX doc_display_maintenance_v1_ring; DROP INDEX doc_display_generations_v1_source`); err != nil {
		test.Fatal(err)
	}
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := holder.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at=due_at`); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0140_doc_display_maintenance_selection.sql")
	if err != nil {
		test.Fatal(err)
	}
	migration := docCatalogAdmissionBeginPG(test, fixture)
	_, err = migration.Exec(fixture.ctx, string(raw))
	docCatalogWriterCodePG(test, err, "55P03")
	rollbackPG(migration)
	var unchanged bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 to_regclass('doc_display_maintenance_cursor_v1') IS NULL AND
 to_regclass('doc_display_maintenance_v1_ring') IS NULL AND
 to_regclass('doc_display_generations_v1_source') IS NULL`).Scan(&unchanged); err != nil || !unchanged {
		test.Fatal("busy selection migration partially installed", unchanged, err)
	}
	if err := holder.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("busy selection migration changed Source/job/maintenance")
	}
	retry := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := retry.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	if err := retry.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); err != nil {
		test.Fatal("migration retry failed to initialize usable ring", err)
	}
}

func TestDocDisplayMaintenanceSelectionPGDoesNotLockJobs(test *testing.T) {
	fixture := docDisplayPG(test)
	target := docCatalogAdmissionTargetPG(fixture, 0, domain.DocDisplayProjectionProfile)
	source := docCatalogAdmissionSourcePG(test, fixture, 0, target.DisplayProfile())
	if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
		test.Fatal(err)
	}
	holder := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := holder.Exec(fixture.ctx, `SELECT repo_id FROM doc_display_jobs_v1 FOR UPDATE`); err != nil {
		test.Fatal(err)
	}
	step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != source.hash || step.CatalogCensus != "protected" || step.CatalogJobs != 1 {
		test.Fatal("collector blocked on a job instead of observing resumable evidence", step, err)
	}
	if err := holder.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
}

func TestDocDisplayMaintenanceSelectionPGDeferralDoesNotWait(test *testing.T) {
	fixture := docDisplayPG(test)
	source := docMaintenanceSelectionSourcesPG(test, fixture, 1)[0]
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE`, source.hash); err != nil {
		test.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(fixture.ctx, 300*time.Millisecond)
	defer cancel()
	fixture.storage.deferDocDisplayMaintenance(bounded, source.hash)
	if err := bounded.Err(); err != nil {
		test.Fatal("integrity deferral waited on maintenance", err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("busy deferral changed candidate state")
	}
	probe := docCatalogAdmissionBeginPG(test, fixture)
	var available bool
	if err := probe.QueryRow(fixture.ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(
 'cxthub-doc-catalog-affinity:'||$1::text||':'||$2::text,0))`, source.event, domain.DocDisplayProjectionProfile).Scan(&available); err != nil || !available {
		test.Fatal("deferral leaked affinity", available, err)
	}
	if _, err := probe.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`, source.hash); err != nil {
		test.Fatal("deferral leaked Source lock", err)
	}
	rollbackPG(probe)
}

func TestDocDisplayMaintenanceSelectionPGFreshAfterFence(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		for _, mutation := range []string{"identity", "bytes", "aba", "reference", "work_idle", "work_future", "delete"} {
			test.Run(profile+"/"+mutation, func(test *testing.T) {
				fixture := docDisplayPG(test)
				source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
				if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, source.hash); err != nil {
					test.Fatal(err)
				}
				previous := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, source.event, profile)
				gate, gatePID := docMaintenanceSelectionGatePG(test, fixture)
				bounded, cancel := context.WithTimeout(fixture.ctx, 3*time.Second)
				defer cancel()
				type result struct {
					step domain.DocDisplayMaintenanceStep
					err  error
				}
				done := make(chan result, 1)
				go func() {
					step, err := fixture.storage.stepDocDisplayMaintenanceCandidate(bounded, source.hash)
					done <- result{step, err}
				}()
				defer func() {
					rollbackPG(gate)
					cancel()
					if done != nil {
						<-done
					}
				}()
				collectorPID := docCatalogEnqueueBlockedPG(test, fixture, gatePID, "SELECT * FROM cxthub_lock_doc_catalog_affinities%")
				var err error
				switch mutation {
				case "identity":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET event_hash=$2 WHERE source_hash=$1`, source.hash, domain.HashContent([]byte("moved maintenance source")))
				case "bytes":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, source.hash, []byte(`{}`))
				case "aba", "delete":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, source.hash)
					if err == nil && mutation == "aba" {
						_, err = docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source)
						if err == nil {
							_, err = fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, source.hash)
						}
					}
				case "reference":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET reference_revision=reference_revision+7 WHERE source_hash=$1`, source.hash)
				case "work_idle":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET phase='idle' WHERE source_hash=$1`, source.hash)
				case "work_future":
					_, err = fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp()+interval '1 day' WHERE source_hash=$1`, source.hash)
				}
				if err != nil {
					test.Fatal(err)
				}
				if err := gate.Rollback(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				observed := <-done
				done = nil
				switch mutation {
				case "identity", "delete", "work_idle", "work_future":
					if !errors.Is(observed.err, errDocDisplayMaintenanceCandidateBusy) {
						test.Fatal("collector consumed a stale identity/queue candidate", mutation, observed)
					}
				case "bytes":
					if !errors.Is(observed.err, domain.ErrIntegrity) {
						test.Fatal("collector validated pre-fence cached bytes", observed)
					}
				default:
					if observed.err != nil || observed.step.Phase != "catalog_census" {
						test.Fatal("fresh source failed census", mutation, observed)
					}
					current := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, source.event, profile)
					var matches bool
					if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT census.generation=admission.generation AND census.admission_revision=admission.admission_revision
 AND census.source_revision=source.reference_revision FROM doc_display_catalog_census_v1 census
 JOIN doc_display_sources_v1 source USING(source_hash)
 JOIN doc_display_catalog_admissions_v1 admission USING(event_hash,profile) WHERE source.source_hash=$1`, source.hash).Scan(&matches); err != nil || !matches {
						test.Fatal("census used stale source revision/generation", matches, err)
					}
					if mutation == "aba" && current.generation == previous.generation {
						test.Fatal("ABA retained deleted Source generation")
					}
				}
				test.Logf("collector PID %d observed at proof-to-fence gate PID %d; mutation=%s", collectorPID, gatePID, mutation)
			})
		}
	}
}

func TestDocDisplayMaintenanceSelectionPGCancelledAfterCommittedPick(test *testing.T) {
	fixture := docDisplayPG(test)
	sources := docMaintenanceSelectionSourcesPG(test, fixture, 2)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	gate, gatePID := docMaintenanceSelectionGatePG(test, fixture)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fixture.storage.StepDocDisplayMaintenance(ctx)
		done <- err
	}()
	defer func() {
		rollbackPG(gate)
		cancel()
		if done != nil {
			<-done
		}
	}()
	collectorPID := docCatalogEnqueueBlockedPG(test, fixture, gatePID, "SELECT * FROM cxthub_lock_doc_catalog_affinities%")
	docMaintenanceSelectionCursorPG(test, fixture, sources[0].hash)
	cancel()
	select {
	case err := <-done:
		done = nil
		if !errors.Is(err, context.Canceled) {
			test.Fatal("post-pick cancellation lost its actual cause", err)
		}
	case <-time.After(time.Second):
		test.Fatal("cancelled collector did not release its blocked attempt")
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("cancelled post-pick attempt changed source/catalog state")
	}
	if err := gate.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	step, err := docMaintenanceSelectionReopenPG(test, fixture).StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != sources[1].hash {
		test.Fatal("restarted collector forgot its committed position", step, err)
	}
	step, err = fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != sources[0].hash {
		test.Fatal("cancelled source was permanently skipped", step, err)
	}
	test.Logf("actual Step cancelled at collector PID %d after committed pick; gate PID %d", collectorPID, gatePID)
}

func TestDocDisplayMaintenanceSelectionPGAbandonedPickerReleasesCursor(test *testing.T) {
	fixture := docDisplayPG(test)
	source := docMaintenanceSelectionSourcesPG(test, fixture, 1)[0]
	configuration := fixture.storage.pool.Config().Copy()
	trace := &docMaintenancePausedPickerPG{selected: make(chan int32, 1), resume: make(chan struct{})}
	configuration.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	stalled := &PostgresStore{pool: pool}
	done := make(chan error, 1)
	go func() {
		_, err := stalled.selectDocDisplayMaintenance(fixture.ctx)
		done <- err
	}()
	defer func() {
		trace.release()
		if done != nil {
			<-done
		}
	}()
	var pickerPID int32
	select {
	case pickerPID = <-trace.selected:
	case <-time.After(time.Second):
		test.Fatal("picker did not reach its locked idle-client boundary")
	}
	bounded, cancel := context.WithTimeout(fixture.ctx, 2*time.Second)
	defer cancel()
	present := true
	for present && bounded.Err() == nil {
		if err := fixture.storage.pool.QueryRow(bounded, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pickerPID).Scan(&present); err != nil {
			test.Fatal(err)
		}
		if present {
			time.Sleep(time.Millisecond)
		}
	}
	if present {
		test.Fatal("server did not terminate abandoned idle picker", pickerPID, bounded.Err())
	}
	docMaintenanceSelectionCursorPG(test, fixture, "")
	step, err := docMaintenanceSelectionReopenPG(test, fixture).StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != source.hash {
		test.Fatal("abandoned selector prevented another server from progressing", step, err)
	}
	trace.release()
	failure := <-done
	done = nil
	if failure == nil {
		test.Fatal("abandoned picker committed after server terminated its transaction")
	}
	test.Logf("idle picker backend PID %d disappeared before client resumed; another pool completed %s", pickerPID, source.hash)
}

func TestDocDisplayMaintenanceSelectionPGPickerTimeoutDoesNotLimitBody(test *testing.T) {
	fixture := docDisplayPG(test)
	source := docMaintenanceSelectionSourcesPG(test, fixture, 1)[0]
	configuration := fixture.storage.pool.Config().Copy()
	configuration.MaxConns = 1
	configuration.MinConns = 0
	pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	storage := &PostgresStore{pool: pool}
	var beforePID int32
	var beforeStatement, beforeIdle string
	if err := pool.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),current_setting('statement_timeout'),current_setting('idle_in_transaction_session_timeout')`).Scan(&beforePID, &beforeStatement, &beforeIdle); err != nil {
		test.Fatal(err)
	}
	gate, gatePID := docMaintenanceSelectionGatePG(test, fixture)
	ctx, cancel := context.WithTimeout(fixture.ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		step, err := storage.StepDocDisplayMaintenance(ctx)
		if err == nil && step.Source != source.hash {
			err = fmt.Errorf("unexpected collector source %s", step.Source)
		}
		done <- err
	}()
	defer func() {
		rollbackPG(gate)
		cancel()
		if done != nil {
			<-done
		}
	}()
	collectorPID := docCatalogEnqueueBlockedPG(test, fixture, gatePID, "SELECT * FROM cxthub_lock_doc_catalog_affinities%")
	if collectorPID != beforePID {
		test.Fatal("timeout-isolation probe did not reuse the same backend", beforePID, collectorPID)
	}
	if _, err := fixture.storage.pool.Exec(ctx, `SELECT pg_sleep(0.6)`); err != nil {
		test.Fatal(err)
	}
	var waiting bool
	if err := fixture.storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE pid=$1 AND $2=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock')`, collectorPID, gatePID).Scan(&waiting); err != nil || !waiting {
		test.Fatal("picker timeout leaked into a live candidate body", waiting, err)
	}
	if err := gate.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	failure := <-done
	done = nil
	if failure != nil {
		test.Fatal("candidate body inherited picker context/settings", failure)
	}
	var afterPID int32
	var afterStatement, afterIdle string
	if err := pool.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),current_setting('statement_timeout'),current_setting('idle_in_transaction_session_timeout')`).Scan(&afterPID, &afterStatement, &afterIdle); err != nil ||
		afterPID != beforePID || afterStatement != beforeStatement || afterIdle != beforeIdle {
		test.Fatal("local picker settings escaped the short transaction", beforePID, afterPID, beforeStatement, afterStatement, beforeIdle, afterIdle, err)
	}
}

func TestDocDisplayMaintenanceSelectionPGMalformedSourceCannotPinCursor(test *testing.T) {
	fixture := docDisplayPG(test)
	healthy := docMaintenanceSelectionSourcesPG(test, fixture, 1)[0]
	corrupt := docCatalogAdmissionSourcePG(test, fixture, 0, domain.DocDisplayProjectionProfile)
	corrupt.hash = "bad-scheduling-key"
	if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, corrupt); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, corrupt.hash); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("malformed Source bypassed canonical/content proof", err)
	}
	docMaintenanceSelectionCursorPG(test, fixture, corrupt.hash)
	step, err := docMaintenanceSelectionReopenPG(test, fixture).StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != healthy.hash {
		test.Fatal("corrupt Source key permanently pinned scheduling position", step, err)
	}
	var count int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1 WHERE source_hash=$1`, corrupt.hash).Scan(&count); err != nil || count != 0 {
		test.Fatal("malformed Source obtained census/delete authority", count, err)
	}
}

func TestDocDisplayMaintenanceSelectionPGFutureRingStopsAfterOneLap(test *testing.T) {
	fixture := docDisplayPG(test)
	docMaintenanceSelectionSourcesPG(test, fixture, 1)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at='infinity'`); err != nil {
		test.Fatal(err)
	}
	configuration := fixture.storage.pool.Config().Copy()
	configuration.MaxConns, configuration.MinConns = 1, 0
	trace := &docDisplayMaintenancePlanTracePG{}
	configuration.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	storage := &PostgresStore{pool: pool}
	if _, err := storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("future-only ring unexpectedly performed candidate work", err)
	}
	if len(trace.queries) != 3 {
		test.Fatal("small future ring repeatedly consumed the entire probe cap", len(trace.queries), trace.queries)
	}
	var census int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1`).Scan(&census); err != nil || census != 0 {
		test.Fatal("future entry caused census work", census, err)
	}
}

func TestDocDisplayMaintenanceSelectionPGConcurrentCursorRebuild(test *testing.T) {
	for _, live := range []bool{false, true} {
		test.Run(fmt.Sprintf("live=%t", live), func(test *testing.T) {
			fixture := docDisplayPG(test)
			if live {
				docMaintenanceSelectionSourcesPG(test, fixture, 2)
			}
			if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_maintenance_cursor_v1`); err != nil {
				test.Fatal(err)
			}
			start := make(chan struct{})
			done := make(chan error, 2)
			for range 2 {
				storage := docMaintenanceSelectionReopenPG(test, fixture)
				go func() {
					<-start
					_, err := storage.StepDocDisplayMaintenance(fixture.ctx)
					done <- err
				}()
			}
			close(start)
			for range 2 {
				if err := <-done; err != nil && !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("concurrent derived cursor rebuild", err)
				}
			}
			var count int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_maintenance_cursor_v1`).Scan(&count); err != nil || count != 1 {
				test.Fatal("derived cursor rebuild lost/duplicated its singleton", count, err)
			}
			if live {
				for range 6 {
					if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); err != nil {
						test.Fatal(err)
					}
				}
				if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1`).Scan(&count); err != nil || count != 2 {
					test.Fatal("repaired cursor failed to revisit all retained work", count, err)
				}
			}
		})
	}
}

func TestDocDisplayMaintenanceSelectionPGRebuildWinnerBoundary(test *testing.T) {
	for _, outcome := range []string{"commit", "rollback", "cancel", "timeout"} {
		test.Run(outcome, func(test *testing.T) {
			fixture := docDisplayPG(test)
			sources := docMaintenanceSelectionSourcesPG(test, fixture, 2)
			if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_maintenance_cursor_v1`); err != nil {
				test.Fatal(err)
			}
			gate := docCatalogAdmissionBeginPG(test, fixture)
			winner := docCatalogAdmissionBeginPG(test, fixture)
			var gatePID, winnerPID int32
			if err := gate.QueryRow(fixture.ctx, `SELECT pg_backend_pid(),pg_advisory_xact_lock(77814003)`).Scan(&gatePID, new(any)); err != nil {
				test.Fatal(err)
			}
			if err := winner.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
				test.Fatal(err)
			}
			if _, err := fixture.storage.pool.Exec(fixture.ctx, fmt.Sprintf(`CREATE FUNCTION test_cursor_rebuild_gate() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF pg_backend_pid()<>%d THEN PERFORM pg_advisory_xact_lock(77814003); END IF; RETURN NEW; END $$;
 CREATE TRIGGER test_cursor_rebuild_gate BEFORE INSERT ON doc_display_maintenance_cursor_v1
 FOR EACH ROW EXECUTE FUNCTION test_cursor_rebuild_gate()`, winnerPID)); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER test_cursor_rebuild_gate ON doc_display_maintenance_cursor_v1;
 DROP FUNCTION test_cursor_rebuild_gate()`); err != nil {
					test.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			type result struct {
				candidate docDisplayMaintenanceCandidate
				err       error
			}
			done := make(chan result, 1)
			go func() {
				candidate, err := fixture.storage.selectDocDisplayMaintenance(ctx)
				done <- result{candidate, err}
			}()
			defer func() {
				rollbackPG(gate)
				rollbackPG(winner)
				cancel()
				if done != nil {
					<-done
				}
			}()
			pickerPID := docCatalogEnqueueBlockedPG(test, fixture, gatePID, "INSERT INTO doc_display_maintenance_cursor_v1%")
			if _, err := winner.Exec(fixture.ctx, `INSERT INTO doc_display_maintenance_cursor_v1(singleton,after_source) VALUES(true,$1)`, sources[0].hash); err != nil {
				test.Fatal(err)
			}
			if err := gate.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if blocked := docCatalogEnqueueBlockedPG(test, fixture, winnerPID, "INSERT INTO doc_display_maintenance_cursor_v1%"); blocked != pickerPID {
				test.Fatal("cursor rebuild did not reach actual invisible uniqueness conflict", blocked, pickerPID)
			}
			probe := docCatalogAdmissionBeginPG(test, fixture)
			if _, err := probe.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 FOR NO KEY UPDATE NOWAIT;
 SELECT source_hash FROM doc_display_maintenance_v1 FOR UPDATE NOWAIT`); err != nil {
				test.Fatal("cursor repair waited while holding candidate resources", err)
			}
			rollbackPG(probe)
			if outcome == "cancel" {
				cancel()
			}
			if outcome == "cancel" || outcome == "timeout" {
				observed := <-done
				done = nil
				var postgresError *pgconn.PgError
				deadline := errors.Is(observed.err, context.DeadlineExceeded) || errors.As(observed.err, &postgresError) && postgresError.Code == "57014"
				if outcome == "cancel" && !errors.Is(observed.err, context.Canceled) || outcome == "timeout" && !deadline {
					test.Fatal("cursor repair ignored cancellation/control deadline", outcome, observed)
				}
			}
			if outcome == "commit" {
				if err := winner.Commit(fixture.ctx); err != nil {
					test.Fatal(err)
				}
			} else if err := winner.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			var selected docDisplayMaintenanceCandidate
			if done != nil {
				observed := <-done
				done = nil
				if observed.err != nil {
					test.Fatal(observed.err)
				}
				selected = observed.candidate
			} else {
				var err error
				selected, err = fixture.storage.selectDocDisplayMaintenance(fixture.ctx)
				if err != nil {
					test.Fatal("bounded failed repair did not recover automatically", err)
				}
			}
			expected := sources[0].hash
			if outcome == "commit" {
				expected = sources[1].hash
			}
			if selected.source != expected {
				test.Fatal("rebuild overwrote committed winner or retained a rolled-back position", outcome, selected.source, expected)
			}
			docMaintenanceSelectionCursorPG(test, fixture, expected)
		})
	}
}

func TestDocDisplayMaintenanceSelectionPGMissingSchemaIsNotRuntimeDDL(test *testing.T) {
	fixture := docDisplayPG(test)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TABLE doc_display_maintenance_cursor_v1`); err != nil {
		test.Fatal(err)
	}
	_, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	docCatalogWriterCodePG(test, err, "42P01")
	var absent bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_regclass('doc_display_maintenance_cursor_v1') IS NULL`).Scan(&absent); err != nil || !absent {
		test.Fatal("collector hid missing schema with runtime DDL", absent, err)
	}
}
