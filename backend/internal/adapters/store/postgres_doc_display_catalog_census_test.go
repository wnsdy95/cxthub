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

type docCatalogCensusFixturePG struct {
	storage  *PostgresStore
	ctx      context.Context
	repo     domain.ContentHash
	doc      domain.ContentHash
	source   domain.DocDisplayCatalogSource
	terminal []byte
	protocol int
}

func docCatalogCensusNewPG(test *testing.T, profile string) docCatalogCensusFixturePG {
	test.Helper()
	storage, ctx := rootProjectionIsolatedPG(test)
	repo, _ := rootPGRepo(test, ctx, storage)
	fixture := docCatalogCensusFixturePG{storage: storage, ctx: ctx, repo: repo, doc: domain.HashContent([]byte(test.Name())), protocol: 5}
	fixture.source = domain.DocDisplayCatalogSource{EventHash: domain.HashContent([]byte(test.Name() + profile)), Profile: profile}
	var err error
	if profile == domain.DocDisplayProjectionProfile {
		source := domain.DocEventDisplaySource{Version: 1, Profile: profile, EventHash: fixture.source.EventHash, EventBytes: 100, Kind: domain.EventReasoning, EmptyReasoning: true}
		fixture.source.Bytes, err = source.Encode()
		if err == nil {
			fixture.terminal, err = (domain.DocEventDisplayPreparation{Version: 1, Source: source, Phase: "completed", Plan: domain.HashContent([]byte("terminal plan")), Generation: domain.HashContent([]byte("terminal generation"))}).Encode()
		}
	} else {
		fixture.protocol = 2
		source := domain.DocDisplayStringSource{Version: 1, Profile: profile, EventHash: fixture.source.EventHash, EventBytes: 100, Token: domain.DocFindSpan{From: 0, To: 2}, Empty: true}
		fixture.source.Bytes, err = source.Encode()
		if err == nil {
			var state domain.DocDisplayStringState
			state, err = domain.NewDocDisplayStringState(source)
			if err == nil {
				fixture.terminal, err = state.Encode(source)
			}
		}
	}
	if err != nil {
		test.Fatal(err)
	}
	fixture.source.Hash = domain.HashContent(fixture.source.Bytes)
	if err := fixture.source.Validate(); err != nil {
		test.Fatal(err)
	}
	docCatalogAdmissionExecPG(test, ctx, storage.pool, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, fixture.source.EventHash, profile, fixture.source.Hash, fixture.source.Bytes)
	if _, err := storage.pool.Exec(ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, fixture.source.Hash); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func (fixture docCatalogCensusFixturePG) suspendJobGuard(test *testing.T, transaction pgx.Tx) func() {
	test.Helper()
	var database string
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()`).Scan(&database); err != nil {
		test.Fatal(err)
	}
	if _, owned := docDisplayOwnedFixturesPG.Load(database); !owned {
		test.Fatal("historical census jobs require an isolated fixture database")
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	return func() {
		test.Helper()
		if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 ENABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
			test.Fatal(err)
		}
	}
}

func (fixture docCatalogCensusFixturePG) corruptJob(test *testing.T, statement string, arguments ...any) {
	test.Helper()
	tag, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, statement, arguments...)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("expected exactly one historical census job mutation", tag, err)
	}
}

func (fixture docCatalogCensusFixturePG) insert(test *testing.T, index int, state string, bound bool, event domain.ContentHash) {
	test.Helper()
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	restore := fixture.suspendJobGuard(test, transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true),set_config('cxthub.doc_catalog_protocol','3',true)`, fmt.Sprint(fixture.protocol)); err != nil {
		test.Fatal(err)
	}
	var source, hash, raw any
	if bound {
		source, hash, raw = fixture.source.Hash, domain.HashContent(fixture.terminal), fixture.terminal
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,event_index,event_hash,profile,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,lease_until,worker_protocol,created_at,next_attempt,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,catalog_epoch)
 VALUES($1,$2,$3,$4,$5,0,100,0,'',gen_random_uuid(),0,$2,$6,
 CASE WHEN $6='running' THEN clock_timestamp()-interval '1 minute' END,$7,clock_timestamp(),clock_timestamp(),$8,
 CASE WHEN $5='tool-result-string-v1' THEN $9::text END,CASE WHEN $5='tool-result-string-v1' THEN $10::bytea END,
 CASE WHEN $5='public-event-display-v1' THEN $9::text END,CASE WHEN $5='public-event-display-v1' THEN $10::bytea END,
 CASE WHEN $5='public-event-display-v1' AND $8::text IS NOT NULL THEN (SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$8) END)`,
		fixture.repo, fixture.doc, index, event, fixture.source.Profile, state, fixture.protocol, source, hash, raw)
	restore()
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
}

func (fixture docCatalogCensusFixturePG) step(test *testing.T) domain.DocDisplayMaintenanceStep {
	test.Helper()
	step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != fixture.source.Hash || step.CatalogJobs > domain.DocDisplayCatalogCensusBatch {
		test.Fatal("bounded source-local census", step, err)
	}
	return step
}

func (fixture docCatalogCensusFixturePG) require(test *testing.T, phase, reason string) docDisplayCatalogCensus {
	test.Helper()
	var result docDisplayCatalogCensus
	err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT generation::text,admission_revision,source_revision,phase,reason,after_repo,after_doc,after_event,after_profile
 FROM doc_display_catalog_census_v1 WHERE source_hash=$1`, fixture.source.Hash).Scan(&result.generation, &result.admission, &result.revision, &result.phase, &result.reason, &result.repo, &result.doc, &result.event, &result.profile)
	if err != nil || result.phase != phase || result.reason != reason {
		test.Fatal("unexpected durable census", result, phase, reason, err)
	}
	return result
}

func (fixture docCatalogCensusFixturePG) clear(test *testing.T) {
	test.Helper()
	for count := 0; count < 8; count++ {
		step := fixture.step(test)
		if step.CatalogCensus == "clear" {
			fixture.require(test, "clear", "")
			return
		}
	}
	test.Fatal("small census did not converge")
}

func TestDocDisplayCatalogCensusPGBoundedResumeAndExplicitOwner(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture := docCatalogCensusNewPG(test, profile)
			count := domain.DocDisplayCatalogCensusBatch*2 + 1
			for index := 0; index < count; index++ {
				fixture.insert(test, index, "completed", true, fixture.source.EventHash)
			}
			for batch := 1; batch <= 2; batch++ {
				step := fixture.step(test)
				state := fixture.require(test, "native", "")
				if step.Phase != "catalog_census" || step.CatalogJobs != domain.DocDisplayCatalogCensusBatch || state.event != batch*domain.DocDisplayCatalogCensusBatch-1 {
					test.Fatal("keyset did not retain exact batch cursor", step, state)
				}
				if batch == 1 {
					storage, err := NewPostgresStore(fixture.ctx, fixture.storage.pool.Config().ConnString())
					if err != nil {
						test.Fatal(err)
					}
					test.Cleanup(storage.Close)
					fixture.storage = storage
				}
			}
			step := fixture.step(test)
			if step.CatalogJobs != 1 || step.CatalogCensus != "explicit" {
				test.Fatal("native tail did not finish independently", step)
			}
			for batch := 0; batch < 3; batch++ {
				step = fixture.step(test)
				if step.CatalogJobs > domain.DocDisplayCatalogCensusBatch || batch < 2 && step.CatalogCensus != "explicit" || batch == 2 && step.CatalogCensus != "clear" {
					test.Fatal("explicit lane exceeded bounded keyset", step)
				}
			}
			before := fixture.require(test, "clear", "")
			fixture.insert(test, count, "completed", true, domain.HashContent([]byte("foreign native event")))
			fixture.step(test)
			fixture.step(test)
			fixture.step(test)
			fixture.step(test)
			fixture.step(test)
			step = fixture.step(test)
			after := fixture.require(test, "protected", "unverified_completion")
			if step.CatalogJobs != 2 || after.admission <= before.admission || after.revision <= before.revision {
				test.Fatal("explicit foreign owner omitted or census not invalidated", before, after, step)
			}
		})
	}
}

func TestDocDisplayCatalogCensusPGUnboundAndResumable(test *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying", "rejected", "completed"} {
		test.Run(state, func(test *testing.T) {
			fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
			fixture.insert(test, 0, state, false, fixture.source.EventHash)
			step := fixture.step(test)
			reason := "resumable_job"
			if state == "completed" {
				reason = "unverified_completion"
			}
			fixture.require(test, "protected", reason)
			if step.CatalogJobs != 1 {
				test.Fatal("unbound job ignored", step)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo)
			fixture.clear(test)
		})
	}
}

func TestDocDisplayCatalogCensusPGInvalidation(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			fixture := docCatalogCensusNewPG(test, profile)
			fixture.insert(test, 0, "rejected", true, fixture.source.EventHash)
			fixture.step(test)
			fixture.require(test, "protected", "resumable_job")
			drainDisplayMaintenance(test, fixture.ctx, fixture.storage)
			fixture.corruptJob(test, `UPDATE doc_display_jobs_v1 SET state='completed' WHERE repo_id=$1`, fixture.repo)
			fixture.clear(test)
			before := fixture.require(test, "clear", "")
			drainDisplayMaintenance(test, fixture.ctx, fixture.storage)
			docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_jobs_v1 SET state=state,event_length=event_length,reason='still completed' WHERE repo_id=$1`, fixture.repo)
			if _, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("same state/length/reason requeued census", err)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_jobs_v1 SET event_length=101 WHERE repo_id=$1`, fixture.repo)
			fixture.step(test)
			after := fixture.require(test, "protected", "unverified_completion")
			if before.admission != after.admission || before.generation != after.generation || after.revision <= before.revision {
				test.Fatal("length-only invalidation used wrong revision", before, after)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_jobs_v1 SET event_length=100 WHERE repo_id=$1`, fixture.repo)
			fixture.clear(test)
			before = fixture.require(test, "clear", "")
			column := "preparation"
			if profile == domain.DocDisplayStringProfile {
				column = "checkpoint"
			}
			fixture.corruptJob(test, `UPDATE doc_display_jobs_v1 SET `+column+`=convert_to('{}','UTF8') WHERE repo_id=$1`, fixture.repo)
			fixture.step(test)
			after = fixture.require(test, "protected", "unverified_completion")
			if before.admission != after.admission || after.revision <= before.revision {
				test.Fatal("raw payload repair failed to invalidate completed census", before, after)
			}
			fixture.corruptJob(test, `UPDATE doc_display_jobs_v1 SET `+column+`=$2 WHERE repo_id=$1`, fixture.repo, fixture.terminal)
			fixture.clear(test)
		})
	}
}

func TestDocDisplayCatalogCensusPGFenceAndBinding(test *testing.T) {
	fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, fixture.source.EventHash, fixture.source.Profile); err != nil {
		test.Fatal(err)
	}
	_, err = fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("busy census candidate was not skipped within its probe budget", err)
	}
	var count int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1`).Scan(&count); err != nil || count != 0 {
		test.Fatal("failed census leaked progress", count, err)
	}
	rollbackPG(transaction)
	fixture.clear(test)
	before := fixture.require(test, "clear", "")
	docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_catalog_admissions_v1 SET source_hash=NULL WHERE event_hash=$1 AND profile=$2`, fixture.source.EventHash, fixture.source.Profile)
	_, err = fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("mismatched admission binding was trusted", err)
	}
	if after := fixture.require(test, "clear", ""); after != before {
		test.Fatal("integrity failure changed prior census", before, after)
	}
}

func TestDocDisplayCatalogCensusPGMigrationQueuesExistingSources(test *testing.T) {
	fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
	drainDisplayMaintenance(test, fixture.ctx, fixture.storage)
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `DROP TABLE doc_display_catalog_census_v1; DROP INDEX doc_display_jobs_v1_catalog_native;
 DROP TRIGGER doc_display_jobs_v1_zz_catalog_census ON doc_display_jobs_v1; DROP FUNCTION cxthub_wake_doc_catalog_census()`); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0137_doc_display_catalog_census.sql")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal(err)
	}
	var phase string
	if err := transaction.QueryRow(fixture.ctx, `SELECT phase FROM doc_display_maintenance_v1 WHERE source_hash=$1`, fixture.source.Hash).Scan(&phase); err != nil || phase != "index" {
		test.Fatal("upgrade failed to queue idle sources", phase, err)
	}
	var count int
	if err := transaction.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1`).Scan(&count); err != nil || count != 0 {
		test.Fatal("migration should not perform unbounded receipt census", count, err)
	}
}

func TestDocDisplayCatalogCensusPGScaleBound(test *testing.T) {
	for _, count := range []int{128, 1280} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
			_, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,event_index,event_hash,profile,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,worker_protocol,created_at,next_attempt,source_hash,preparation_hash,preparation,catalog_epoch)
 SELECT $1,$2,ordinal,$3,$4,0,100,0,'',gen_random_uuid(),0,$2,'completed',5,clock_timestamp(),clock_timestamp(),$5,$6,$7,
 (SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$5)
 FROM generate_series(0,$8::integer-1) entry(ordinal)`, fixture.repo, fixture.doc, fixture.source.EventHash, fixture.source.Profile,
				fixture.source.Hash, domain.HashContent(fixture.terminal), fixture.terminal, count)
			if err != nil {
				test.Fatal(err)
			}
			step := fixture.step(test)
			state := fixture.require(test, "native", "")
			if step.CatalogJobs != domain.DocDisplayCatalogCensusBatch || state.event != domain.DocDisplayCatalogCensusBatch-1 || step.Indexed != 0 || step.Marked != 0 || step.Deleted != 0 {
				test.Fatal("census batch grew with total history or shared GC budget", step, state)
			}
		})
	}
}

func TestDocDisplayCatalogCensusPGFairQueue(test *testing.T) {
	fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
	for index := 0; index < domain.DocDisplayCatalogCensusBatch*2+1; index++ {
		fixture.insert(test, index, "completed", true, fixture.source.EventHash)
	}
	other, _ := maintenanceSource(test, fixture.ctx, fixture.storage, "other source progresses during census")
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, other); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp()-CASE WHEN source_hash=$1 THEN interval '2 minutes' ELSE interval '1 minute' END`, fixture.source.Hash); err != nil {
		test.Fatal(err)
	}
	after := ""
	if fixture.source.Hash > other {
		after = string(other)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_display_maintenance_cursor_v1 SET after_source=$1 WHERE singleton`, after); err != nil {
		test.Fatal(err)
	}
	first := fixture.step(test)
	second, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || first.CatalogJobs != domain.DocDisplayCatalogCensusBatch || second.Source != other || second.Phase != "catalog_census" {
		test.Fatal("large census retained the oldest queue position", first, second, err)
	}
	fixture.require(test, "native", "")
}

func TestDocDisplayCatalogCensusPGSourceABA(test *testing.T) {
	fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
	fixture.clear(test)
	before := fixture.require(test, "clear", "")
	docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, fixture.source.Hash)
	var count int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_catalog_census_v1 WHERE source_hash=$1`, fixture.source.Hash).Scan(&count); err != nil || count != 0 {
		test.Fatal("deleted source retained census", count, err)
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, fixture.source.EventHash, fixture.source.Profile, fixture.source.Hash, fixture.source.Bytes)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, fixture.source.Hash); err != nil {
		test.Fatal(err)
	}
	fixture.clear(test)
	after := fixture.require(test, "clear", "")
	if before.generation == after.generation {
		test.Fatal("source ABA reused stale census generation", before, after)
	}
}

func TestDocDisplayCatalogCensusPGWakeSourceOrder(test *testing.T) {
	fixture := docCatalogCensusNewPG(test, domain.DocDisplayProjectionProfile)
	var other domain.DocDisplayCatalogSource
	for index := 0; index < 100; index++ {
		event := domain.HashContent([]byte(fmt.Sprintf("opposite-source-order-%d", index)))
		value := domain.DocEventDisplaySource{Version: 1, Profile: fixture.source.Profile, EventHash: event, EventBytes: 100, Kind: domain.EventReasoning, EmptyReasoning: true}
		raw, err := value.Encode()
		if err != nil {
			test.Fatal(err)
		}
		candidate := domain.DocDisplayCatalogSource{EventHash: event, Profile: value.Profile, Hash: domain.HashContent(raw), Bytes: raw}
		if (fixture.source.EventHash < event) != (fixture.source.Hash < candidate.Hash) {
			other = candidate
			break
		}
	}
	if other.Hash == "" {
		test.Fatal("failed to construct opposed affinity/source orders")
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, other.EventHash, other.Profile, other.Hash, other.Bytes)
	fixture.insert(test, 0, "waiting", true, other.EventHash)
	first, second := fixture.source.Hash, other.Hash
	if first > second {
		first, second = second, first
	}
	holder, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(holder)
	var holderID int
	if err := holder.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&holderID); err != nil {
		test.Fatal(err)
	}
	if _, err := holder.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE`, first); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(fixture.ctx, 3*time.Second)
	defer cancel()
	writer, err := fixture.storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(writer)
	restore := fixture.suspendJobGuard(test, writer)
	result := make(chan error, 1)
	go func() {
		_, err := writer.Exec(ctx, `UPDATE doc_display_jobs_v1 SET state='completed' WHERE repo_id=$1`, fixture.repo)
		result <- err
	}()
	observed := false
	for !observed && ctx.Err() == nil {
		if err := fixture.storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity activity
 WHERE activity.pid<>pg_backend_pid() AND $1=ANY(pg_blocking_pids(activity.pid)))`, holderID).Scan(&observed); err != nil {
			test.Fatal(err)
		}
		if !observed {
			select {
			case err := <-result:
				test.Fatal("wake did not wait on first maintenance row", err)
			case <-time.After(time.Millisecond):
			}
		}
	}
	if !observed {
		test.Fatal("maintenance blocker not observed", ctx.Err())
	}
	probe, err := fixture.storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(probe)
	var available domain.ContentHash
	probeErr := probe.QueryRow(ctx, `SELECT source_hash FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE NOWAIT`, second).Scan(&available)
	rollbackPG(probe)
	rollbackPG(holder)
	if err := <-result; err != nil {
		test.Fatal("wake failed after ordered lock release", err)
	}
	if probeErr != nil || available != second {
		rollbackPG(writer)
		test.Fatal("wake took later source before blocked earlier source", available, probeErr)
	}
	restore()
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}
