//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docRetirementCommitPG struct {
	pid    uint32
	resume chan struct{}
}

type docRetirementTracePG struct {
	commits   chan docRetirementCommitPG
	queries   []string
	arguments [][]any
}

func (trace *docRetirementTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "DELETE FROM doc_tool_key_observations_v1") {
		trace.queries = append(trace.queries, data.SQL)
		trace.arguments = append(trace.arguments, append([]any{}, data.Args...))
	}
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		gate := docRetirementCommitPG{pid: connection.PgConn().PID(), resume: make(chan struct{})}
		select {
		case trace.commits <- gate:
		case <-ctx.Done():
			return ctx
		}
		select {
		case <-gate.resume:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*docRetirementTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func docRetirementCollectorPG(test *testing.T, fixture docDisplayPGFixture) (*PostgresStore, *docRetirementTracePG) {
	test.Helper()
	configuration, err := pgxpool.ParseConfig(fixture.storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	trace := &docRetirementTracePG{commits: make(chan docRetirementCommitPG)}
	configuration.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}, trace
}

func docRetirementOutputPG(count int) map[string]any {
	output := make(map[string]any, count)
	for index := 0; index < count; index++ {
		output[fmt.Sprintf("key_%03d", index)] = true
	}
	return output
}

func docRetirementCompletedPG(test *testing.T, output map[string]any, corrupt func(docDisplayPGFixture, domain.DocDisplayJob)) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	isolated, _ := rootProjectionIsolatedPG(test)
	fixture, job, prepared := docToolOrdersPGStart(test, output)
	if fixture.storage.pool.Config().ConnConfig.Database != isolated.pool.Config().ConnConfig.Database {
		test.Fatal("retirement workflow escaped its owned isolated database")
	}
	prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
	for turns := 0; turns < 256 && prepared.Phase != "completed"; turns++ {
		var step domain.DocEventDisplayStep
		err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
			if err != nil {
				return err
			}
			reader := work.(*postgresDocEventDisplayWork)
			tools := docCatalogEpochToolsPG(reader, prepared)
			tools.OrderPlan = func(read context.Context, parent int) (domain.DocToolOrderPlan, error) {
				return reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders, parent)
			}
			step, err = reader.PrepareStep(read, prepared, docCatalogEpochRawPG(fixture, job), tools)
			return err
		})
		if err != nil {
			test.Fatal("calculate the actual tool publication", prepared.Phase, err)
		}
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("save the actual tool publication", err)
		}
	}
	if prepared.Phase != "completed" {
		test.Fatal("bounded actual tool workflow did not complete", prepared.Phase)
	}
	for _, table := range docCatalogWriterTablesPG {
		var count int
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count == 0 {
			test.Fatal("normal tool workflow must populate every private catalog", table, count, err)
		}
	}
	if corrupt != nil {
		corrupt(fixture, job)
	}
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, docCatalogUseCheckpointPG(test, prepared), time.Now().UTC()); err != nil {
		test.Fatal("publish the actual completed tool receipt", err)
	}
	job.State = "completed"
	docCatalogProgressGuardsPG(test, fixture)
	return fixture, job, prepared
}

func docRetirementAuditPG(test *testing.T, fixture docDisplayPGFixture) {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	var owned bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()=$1`, fixture.storage.pool.Config().ConnConfig.Database).Scan(&owned); err != nil || !owned {
		test.Fatal("retirement observer requires its actual owned database", owned, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `CREATE TABLE cxthub_fixture_doc_retirement_audit(
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,transaction_id bigint NOT NULL,
 relation_name text NOT NULL,operation text NOT NULL,depth integer NOT NULL,old_row jsonb);
 CREATE FUNCTION cxthub_fixture_observe_doc_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
   INSERT INTO cxthub_fixture_doc_retirement_audit(transaction_id,relation_name,operation,depth,old_row)
   VALUES(txid_current(),TG_TABLE_NAME,TG_OP,pg_trigger_depth(),CASE WHEN TG_OP='DELETE' THEN to_jsonb(OLD) END);
   RETURN NULL;
 END; $$`); err != nil {
		test.Fatal(err)
	}
	tables := append([]string{}, docCatalogWriterTablesPG...)
	tables = append(tables, "doc_display_jobs_v1", "doc_display_sources_v1", "doc_display_catalog_admissions_v1",
		"doc_display_catalog_lifecycle_v1", "doc_display_catalog_census_v1", "doc_display_maintenance_v1",
		"doc_display_maintenance_cursor_v1", "doc_display_objects_v1", "doc_display_generations_v1",
		"doc_display_object_edges_v1", "doc_display_object_marks_v1")
	for _, table := range tables {
		if _, err := transaction.Exec(fixture.ctx, `CREATE TRIGGER cxthub_fixture_observe_doc_retirement
 AFTER INSERT OR UPDATE OR DELETE ON `+pgx.Identifier{table}.Sanitize()+`
 FOR EACH ROW EXECUTE FUNCTION cxthub_fixture_observe_doc_retirement()`); err != nil {
			test.Fatal(err)
		}
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
}

func docRetirementStepPG(test *testing.T, fixture docDisplayPGFixture, collector *PostgresStore, trace *docRetirementTracePG) (domain.DocDisplayMaintenanceStep, error) {
	test.Helper()
	var retiring bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_catalog_lifecycle_v1 WHERE state='retiring')`).Scan(&retiring); err != nil {
		test.Fatal(err)
	}
	trace.queries, trace.arguments = nil, nil
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `TRUNCATE cxthub_fixture_doc_retirement_audit RESTART IDENTITY`); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	type outcome struct {
		step domain.DocDisplayMaintenanceStep
		err  error
	}
	finished := make(chan outcome, 1)
	go func() {
		step, err := collector.StepDocDisplayMaintenance(ctx)
		finished <- outcome{step, err}
	}()
	for {
		select {
		case gate := <-trace.commits:
			var unlocked bool
			err := fixture.storage.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_locks
 WHERE pid=$1 AND relation='doc_display_jobs_v1'::regclass AND granted AND mode<>'AccessShareLock')`, gate.pid).Scan(&unlocked)
			close(gate.resume)
			if err != nil || !unlocked {
				test.Fatal("actual collector acquired a job lock before commit; only AccessShareLock is allowed", gate.pid, unlocked, err)
			}
		case result := <-finished:
			var mutations, deleted, nested, unexpected int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*),
 count(*) FILTER(WHERE relation_name=ANY($1::text[]) AND operation='DELETE'),
 count(*) FILTER(WHERE relation_name=ANY($1::text[]) AND operation='DELETE' AND depth<>1),
 count(*) FILTER(WHERE relation_name=ANY($1::text[]) AND operation<>'DELETE')
 FROM cxthub_fixture_doc_retirement_audit`, docCatalogWriterTablesPG).Scan(&mutations, &deleted, &nested, &unexpected); err != nil {
				test.Fatal(err)
			}
			if deleted != result.step.CatalogDeleted || deleted > 16 || nested != 0 || unexpected != 0 {
				test.Fatal("private deletion accounting omitted physical rows, used a cascade, or exceeded the fixed batch", result.step, deleted, nested, unexpected)
			}
			if (retiring || result.step.CatalogPhase != "") && mutations > 64 {
				test.Fatal("retirement exceeded its total physical row-mutation budget including nested queue/FK triggers", result.step, mutations)
			}
			return result.step, result.err
		case <-ctx.Done():
			test.Fatal("actual retirement step exceeded its inherited fixture deadline", ctx.Err())
		}
	}
}

func docRetirementCountsPG(test *testing.T, fixture docDisplayPGFixture) []int {
	test.Helper()
	counts := make([]int, len(docCatalogWriterTablesPG))
	for index, table := range docCatalogWriterTablesPG {
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&counts[index]); err != nil {
			test.Fatal(err)
		}
	}
	return counts
}

func docRetirementStatePG(test *testing.T, fixture docDisplayPGFixture, source domain.ContentHash) (string, string, string) {
	test.Helper()
	var state, epoch, image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,catalog_epoch::text,to_jsonb(lifecycle)::text
 FROM doc_display_catalog_lifecycle_v1 lifecycle WHERE source_hash=$1`, source).Scan(&state, &epoch, &image); err != nil || domain.ValidateDocDisplayCatalogEpoch(epoch) != nil {
		test.Fatal("collector lost the actual lifecycle or its nonzero epoch", state, epoch, err)
	}
	return state, epoch, image
}

func docRetirementPublicationPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) string {
	test.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'source',to_jsonb(source)-'reference_revision','admission_generation',admission.generation,
 'job',to_jsonb(job),'publication',to_jsonb(publication))::text
 FROM doc_display_jobs_v1 job JOIN doc_display_sources_v1 source ON source.source_hash=job.source_hash
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash AND admission.profile=source.profile
 JOIN doc_display_generations_v1 publication ON publication.source_hash=source.source_hash AND publication.event_hash=source.event_hash
 AND publication.profile=source.profile WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.event_index=$3 AND job.profile=$4`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).Scan(&image); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.ReadDocEventDisplayGeneration(read, job.Target)
		if err != nil {
			return err
		}
		raw, err := generation.Encode()
		if err != nil || domain.HashContent(raw) != prepared.Generation || generation.Source.Plan != prepared.Plan {
			return domain.ErrIntegrity
		}
		for page := 0; page < generation.Pages.Count; page++ {
			if _, err := fixture.storage.ReadDocEventDisplayPage(read, job.Target, page); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		test.Fatal("private cleanup invalidated an actual completed published CAS read", err)
	}
	return image
}

func docRetirementDrainPG(test *testing.T, fixture docDisplayPGFixture, collector *PostgresStore, trace *docRetirementTracePG, source domain.ContentHash, maximum int) int {
	test.Helper()
	deleted := 0
	for turn := 0; turn < maximum; turn++ {
		state, _, _ := docRetirementStatePG(test, fixture, source)
		if state == "retired" {
			for _, count := range docRetirementCountsPG(test, fixture) {
				if count != 0 {
					test.Fatal("retired lifecycle retained private rows")
				}
			}
			return deleted
		}
		step, err := docRetirementStepPG(test, fixture, collector, trace)
		if err != nil || step.Source != source {
			test.Fatal("actual automatic retirement stopped before its durable terminal state", turn, step, err)
		}
		deleted += step.CatalogDeleted
	}
	test.Fatal("private retirement did not converge within its row-derived step bound")
	return 0
}

func docRetirementHoldCompletedJobPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) pgx.Tx {
	test.Helper()
	transaction := docCatalogUseBeginPG(test, fixture)
	var completed bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT state='completed' FROM doc_display_jobs_v1
 WHERE `+docDisplayKey(job.Target)+` FOR UPDATE NOWAIT`, job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&completed); err != nil || !completed {
		test.Fatal("independent completed job row owner was not established", completed, err)
	}
	return transaction
}

func TestDocDisplayRetirementPGCompletedToolCatalogBoundedAndNoJobLocks(test *testing.T) {
	fixture, job, prepared := docRetirementCompletedPG(test, docRetirementOutputPG(24), nil)
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	counts := docRetirementCountsPG(test, fixture)
	initial := 0
	for _, count := range counts {
		initial += count
	}
	state, epoch, _ := docRetirementStatePG(test, fixture, source)
	if state != "live" || initial <= 16 {
		test.Fatal("actual normal completion did not leave a live multi-batch private catalog", state, counts)
	}
	before := docRetirementPublicationPG(test, fixture, job, prepared)
	docRetirementAuditPG(test, fixture)
	collector, trace := docRetirementCollectorPG(test, fixture)
	blocker := docRetirementHoldCompletedJobPG(test, fixture, job)
	defer rollbackPG(blocker)
	deleted := docRetirementDrainPG(test, fixture, collector, trace, source, initial*2+64)
	_, terminalEpoch, _ := docRetirementStatePG(test, fixture, source)
	if deleted != initial || terminalEpoch != epoch || docRetirementPublicationPG(test, fixture, job, prepared) != before {
		test.Fatal("private retirement changed Source incarnation/completed CAS or failed to account for every deleted row", deleted, initial, epoch, terminalEpoch)
	}
	test.Logf("normal_tool_keys=24 initial_private_rows=%d private_batch_limit=16 total_mutation_limit=64", initial)
}

func TestDocDisplayRetirementPGNestedOrderPlanMutationBudget(test *testing.T) {
	output := make(map[string]any, 24)
	for index := 0; index < 24; index++ {
		output[fmt.Sprintf("nested_%03d", index)] = []any{true}
	}
	fixture, job, prepared := docRetirementCompletedPG(test, output, nil)
	source, _ := prepared.Source.Hash()
	counts := docRetirementCountsPG(test, fixture)
	initial := 0
	for _, count := range counts {
		initial += count
	}
	if counts[len(counts)-1] != 25 {
		test.Fatal("normal nested workflow must persist root plus 24 nested order plans", counts)
	}
	before := docRetirementPublicationPG(test, fixture, job, prepared)
	docRetirementAuditPG(test, fixture)
	collector, trace := docRetirementCollectorPG(test, fixture)
	blocker := docRetirementHoldCompletedJobPG(test, fixture, job)
	defer rollbackPG(blocker)
	fullBatch, retired, deleted := false, false, 0
	for turn := 0; turn < initial*2+64; turn++ {
		state, _, _ := docRetirementStatePG(test, fixture, source)
		if state == "retired" {
			retired = true
			break
		}
		step, err := docRetirementStepPG(test, fixture, collector, trace)
		if err != nil || step.Source != source {
			test.Fatal("nested order-plan cleanup did not continue normally", step, err)
		}
		deleted += step.CatalogDeleted
		var plans, mutations int
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 count(*) FILTER(WHERE relation_name='doc_tool_order_plans_v1' AND operation='DELETE'),count(*)
 FROM cxthub_fixture_doc_retirement_audit`).Scan(&plans, &mutations); err != nil {
			test.Fatal(err)
		}
		if plans == 16 {
			if step.CatalogPhase != "orders" || step.CatalogDeleted != 16 || mutations > 64 || mutations <= plans {
				test.Fatal("full nested plan batch omitted its nested queue writes or exceeded the total physical mutation budget", step, plans, mutations)
			}
			fullBatch = true
			test.Logf("actual_order_plan_deletes=16 actual_total_row_mutations=%d total_mutation_limit=64", mutations)
		}
	}
	if !fullBatch || !retired || deleted != initial {
		test.Fatal("nested fixture did not exercise a full order batch and complete every private deletion", fullBatch, retired, deleted, initial)
	}
	for _, count := range docRetirementCountsPG(test, fixture) {
		if count != 0 {
			test.Fatal("nested retirement retained private rows")
		}
	}
	if docRetirementPublicationPG(test, fixture, job, prepared) != before {
		test.Fatal("nested plan cleanup changed the actual completed publication")
	}
}

func TestDocDisplayRetirementPGRestartNewAdmissionAndCompletedReuse(test *testing.T) {
	fixture, job, prepared := docRetirementCompletedPG(test, docRetirementOutputPG(32), nil)
	source, _ := prepared.Source.Hash()
	before := docRetirementPublicationPG(test, fixture, job, prepared)
	_, originalEpoch, _ := docRetirementStatePG(test, fixture, source)
	docRetirementAuditPG(test, fixture)
	collector, trace := docRetirementCollectorPG(test, fixture)
	blocker := docRetirementHoldCompletedJobPG(test, fixture, job)
	defer rollbackPG(blocker)
	firstDeletion := false
	for turn := 0; turn < 128; turn++ {
		step, err := docRetirementStepPG(test, fixture, collector, trace)
		if err != nil || step.Source != source {
			test.Fatal("could not reach the first committed private deletion", step, err)
		}
		if step.CatalogDeleted > 0 {
			firstDeletion = true
			break
		}
	}
	state, epoch, durable := docRetirementStatePG(test, fixture, source)
	if !firstDeletion || state != "retiring" || epoch != originalEpoch {
		test.Fatal("restart boundary must be after a real deletion and before retirement completes", firstDeletion, state, epoch)
	}
	collector.pool.Close()
	collector, trace = docRetirementCollectorPG(test, fixture)
	_, _, reopened := docRetirementStatePG(test, fixture, source)
	if reopened != durable {
		test.Fatal("new collector connection changed the committed retirement cursor")
	}
	pending := docDisplayPG(test, fixture.doc.cir.Events...)
	pending.ctx = fixture.ctx
	pendingJob := enqueueDocEventDisplayQueue(test, pending, 0)
	pendingClaim := pending.claim(test)
	receiver := docDisplayPG(test, fixture.doc.cir.Events...)
	receiver.ctx = fixture.ctx
	enqueueDocEventDisplayQueue(test, receiver, 0)
	reuseJob := receiver.claim(test)
	adopted, err := receiver.storage.LoadDocEventDisplay(receiver.ctx, reuseJob)
	if err != nil || !reflect.DeepEqual(adopted, prepared) {
		test.Fatal("real completed CAS could not be adopted during partial private cleanup", err)
	}
	if historical := docCatalogJobEpochPG(test, receiver, reuseJob); historical != originalEpoch {
		test.Fatal("completed reuse relabeled its historical private epoch", historical, originalEpoch)
	}
	if err := receiver.storage.CompleteDocEventDisplay(receiver.ctx, reuseJob, docCatalogUseCheckpointPG(test, adopted), time.Now().UTC()); err != nil {
		test.Fatal("completed CAS publication required private cleanup to stop", err)
	}
	state, epoch, _ = docRetirementStatePG(test, fixture, source)
	if state != "retiring" || epoch != originalEpoch {
		test.Fatal("completed CAS reuse reactivated or relabeled the retiring catalog", state, epoch)
	}
	pendingClaim.State = "waiting"
	if err := pending.storage.FinishDocDisplay(pending.ctx, pendingClaim, time.Now().UTC()); err != nil {
		test.Fatal("return the earlier real claim to waiting before restarting cleanup", err)
	}
	var waiting string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job
 WHERE `+docDisplayKey(pendingJob.Target), pendingJob.RepoID, pendingJob.DocHash, pendingJob.Target.Event.Index).Scan(&waiting); err != nil {
		test.Fatal(err)
	}
	docRetirementDrainPG(test, fixture, collector, trace, source, 512)
	var afterWaiting string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job
 WHERE `+docDisplayKey(pendingJob.Target), pendingJob.RepoID, pendingJob.DocHash, pendingJob.Target.Event.Index).Scan(&afterWaiting); err != nil || afterWaiting != waiting {
		test.Fatal("retirement rewrote or stranded the newly admitted waiting job", err)
	}
	claim := pending.claim(test)
	loaded, err := pending.storage.LoadDocEventDisplay(pending.ctx, claim)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("waiting admission could not proceed after durable cleanup", err)
	}
	if err := pending.storage.CompleteDocEventDisplay(pending.ctx, claim, docCatalogUseCheckpointPG(test, loaded), time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if docRetirementPublicationPG(test, fixture, job, prepared) != before {
		test.Fatal("restart, new admission, or completed reuse changed the original completed evidence")
	}
}

type docRetirementKeyGroupPG struct {
	scope, digest       string
	parent, first, last int64
}

func docRetirementKeyFixturePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, endpoint string, duplicateCount int) docRetirementKeyGroupPG {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	if endpoint != "first_from" && endpoint != "last_from" && (endpoint != "duplicate_group" || duplicateCount < 3) {
		test.Fatal("unsupported narrowly scoped key retirement fixture", endpoint, duplicateCount)
	}
	var group docRetirementKeyGroupPG
	err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		var owned bool
		if err := transaction.QueryRow(bound, `SELECT current_database()=$1`, fixture.storage.pool.Config().ConnConfig.Database).Scan(&owned); err != nil || !owned {
			return fmt.Errorf("key retirement fixture transaction escaped its owned database: owned=%t: %w", owned, err)
		}
		const triggerImage = `SELECT bool_and(tgenabled='O'),jsonb_agg(jsonb_build_array(tgrelid::regclass::text,tgname,tgenabled::text)
 ORDER BY tgrelid::regclass::text,tgname)::text FROM pg_trigger stored JOIN pg_class relation ON relation.oid=stored.tgrelid
 WHERE relation.relnamespace=current_schema()::regnamespace`
		var original, restored string
		var enabled bool
		if err := transaction.QueryRow(bound, triggerImage).Scan(&enabled, &original); err != nil || !enabled {
			return fmt.Errorf("key retirement fixture requires every original guard enabled: enabled=%t: %w", enabled, err)
		}
		if err := transaction.QueryRow(bound, `SELECT member.scope,member.parent_from,member.digest,member.first_from,member.last_from
 FROM doc_tool_key_members_v1 member JOIN doc_tool_key_sources_v1 keys ON keys.scope=member.scope
 JOIN doc_tool_tree_sources_v1 tree ON tree.scope=keys.tree_scope JOIN doc_display_jobs_v1 job ON job.source_hash=tree.source_hash
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.event_index=$3 AND job.profile=$4
 ORDER BY member.scope,member.parent_from,member.digest LIMIT 1`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).
			Scan(&group.scope, &group.parent, &group.digest, &group.first, &group.last); err != nil {
			return err
		}
		if _, err := transaction.Exec(bound, `ALTER TABLE doc_tool_key_members_v1 DISABLE TRIGGER doc_tool_key_members_v1_complete_guard`); err != nil {
			return err
		}
		if endpoint == "duplicate_group" {
			var digests []string
			if err := transaction.QueryRow(bound, `SELECT array_agg(digest ORDER BY digest),min(first_from),max(last_from)
 FROM (SELECT digest,first_from,last_from FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2
 ORDER BY digest LIMIT $3) members`, group.scope, group.parent, duplicateCount).Scan(&digests, &group.first, &group.last); err != nil {
				return err
			}
			if len(digests) != duplicateCount || digests[0] != group.digest || group.first >= group.last {
				return domain.ErrIntegrity
			}
			if _, err := transaction.Exec(bound, `ALTER TABLE doc_tool_key_observations_v1 DISABLE TRIGGER doc_tool_key_observations_v1_complete_guard`); err != nil {
				return err
			}
			tag, err := transaction.Exec(bound, `UPDATE doc_tool_key_observations_v1 SET digest=$3
 WHERE scope=$1 AND parent_from=$2 AND digest=ANY($4::text[])`, group.scope, group.parent, group.digest, digests)
			if err != nil || tag.RowsAffected() != int64(duplicateCount) {
				return fmt.Errorf("coalesce only the selected sibling observations: rows=%d: %w", tag.RowsAffected(), err)
			}
			if _, err := transaction.Exec(bound, `UPDATE doc_tool_key_members_v1 SET first_from=$4,last_from=$5
 WHERE scope=$1 AND parent_from=$2 AND digest=$3`, group.scope, group.parent, group.digest, group.first, group.last); err != nil {
				return err
			}
			if _, err := transaction.Exec(bound, `ALTER TABLE doc_tool_key_members_v1 DISABLE TRIGGER doc_tool_key_members_v1_a_catalog_delete`); err != nil {
				return err
			}
			tag, err = transaction.Exec(bound, `DELETE FROM doc_tool_key_members_v1
 WHERE scope=$1 AND parent_from=$2 AND digest<>$3 AND digest=ANY($4::text[])`, group.scope, group.parent, group.digest, digests)
			if err != nil || tag.RowsAffected() != int64(duplicateCount-1) {
				return fmt.Errorf("remove only the coalesced duplicate member fixtures: rows=%d: %w", tag.RowsAffected(), err)
			}
			if _, err := transaction.Exec(bound, `SET CONSTRAINTS ALL IMMEDIATE;
 ALTER TABLE doc_tool_key_members_v1 ENABLE TRIGGER doc_tool_key_members_v1_a_catalog_delete;
 ALTER TABLE doc_tool_key_observations_v1 ENABLE TRIGGER doc_tool_key_observations_v1_complete_guard`); err != nil {
				return err
			}
		} else {
			predicate := "node_from<$4"
			if endpoint == "last_from" {
				predicate = "node_from>$4"
			}
			var foreign int64
			if err := transaction.QueryRow(bound, `SELECT node_from FROM doc_tool_key_observations_v1
 WHERE scope=$1 AND parent_from=$2 AND digest<>$3 AND `+predicate+` ORDER BY node_from LIMIT 1`, group.scope, group.parent, group.digest, group.first).Scan(&foreign); err != nil {
				return err
			}
			tag, err := transaction.Exec(bound, `UPDATE doc_tool_key_members_v1 SET `+pgx.Identifier{endpoint}.Sanitize()+`=$4
 WHERE scope=$1 AND parent_from=$2 AND digest=$3`, group.scope, group.parent, group.digest, foreign)
			if err != nil || tag.RowsAffected() != 1 {
				return fmt.Errorf("replace exactly one FK-valid foreign group endpoint: rows=%d: %w", tag.RowsAffected(), err)
			}
		}
		if _, err := transaction.Exec(bound, `SET CONSTRAINTS ALL IMMEDIATE;
 ALTER TABLE doc_tool_key_members_v1 ENABLE TRIGGER doc_tool_key_members_v1_complete_guard`); err != nil {
			return err
		}
		if err := transaction.QueryRow(bound, triggerImage).Scan(&enabled, &restored); err != nil || !enabled || original != restored {
			return fmt.Errorf("key retirement fixture failed to restore every original guard: enabled=%t: %w", enabled, err)
		}
		return nil
	})
	if err != nil {
		test.Fatal("construct the explicitly owned key-retirement fixture with real calculation binding and restored guards", endpoint, err)
	}
	return group
}

func TestDocDisplayRetirementPGLargeDuplicateGroupAndEndpointDeletion(test *testing.T) {
	var group docRetirementKeyGroupPG
	fixture, job, prepared := docRetirementCompletedPG(test, docRetirementOutputPG(144), func(fixture docDisplayPGFixture, job domain.DocDisplayJob) {
		group = docRetirementKeyFixturePG(test, fixture, job, "duplicate_group", 97)
	})
	source, _ := prepared.Source.Hash()
	before := docRetirementPublicationPG(test, fixture, job, prepared)
	docRetirementAuditPG(test, fixture)
	collector, trace := docRetirementCollectorPG(test, fixture)
	blocker := docRetirementHoldCompletedJobPG(test, fixture, job)
	defer rollbackPG(blocker)
	var previous, groups int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM doc_tool_key_observations_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3),
 (SELECT count(*) FROM doc_tool_key_members_v1 WHERE scope=$1)`, group.scope, group.parent, group.digest).Scan(&previous, &groups); err != nil || previous != 97 || groups != 48 {
		test.Fatal("large duplicate fixture lost its oversized group or independent sibling groups", previous, groups, err)
	}
	interiorBatches, removed := 0, false
	for turn := 0; turn < 256; turn++ {
		step, err := docRetirementStepPG(test, fixture, collector, trace)
		if err != nil {
			test.Fatal("actual collector failed on an FK-valid large duplicate group", step, err)
		}
		var observations, endpoints int
		var member bool
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM doc_tool_key_observations_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3),
 (SELECT count(*) FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from IN ($4,$5)),
 EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2 AND digest=$3)`,
			group.scope, group.parent, group.digest, group.first, group.last).Scan(&observations, &endpoints, &member); err != nil {
			test.Fatal(err)
		}
		if observations > 0 {
			if !member || endpoints != 2 || observations > previous {
				test.Fatal("interior cleanup prematurely deleted a referenced endpoint/member or added rows", observations, endpoints, member)
			}
			if observations < previous {
				if step.CatalogDeleted != previous-observations || previous-observations > 16 {
					test.Fatal("interior duplicate cleanup exceeded or misreported its exact row batch", step, previous, observations)
				}
				interiorBatches++
				matched := false
				for index, query := range trace.queries {
					if strings.Contains(query, "LIMIT") {
						if !strings.Contains(query, "parent_from=$2 AND digest=$3") || !strings.Contains(query, "node_from<>$4 AND node_from<>$5") ||
							!reflect.DeepEqual(trace.arguments[index], []any{group.scope, group.parent, group.digest, group.first, group.last, 16}) {
							test.Fatal("actual interior deletion omitted its exact group, endpoints, or fixed limit", query, trace.arguments[index])
						}
						matched = true
					}
				}
				if !matched {
					test.Fatal("no actual bounded group deletion was observed")
				}
			}
			previous = observations
			continue
		}
		if member || endpoints != 0 || previous != 2 || step.CatalogDeleted != 3 {
			test.Fatal("final member and both endpoints were not deleted together after all interiors", member, endpoints, previous, step)
		}
		var atomic bool
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(DISTINCT transaction_id)=1
 AND count(*) FILTER(WHERE relation_name='doc_tool_key_members_v1')=1
 AND count(*) FILTER(WHERE relation_name='doc_tool_key_observations_v1')=2
 AND min(sequence) FILTER(WHERE relation_name='doc_tool_key_members_v1')<min(sequence) FILTER(WHERE relation_name='doc_tool_key_observations_v1')
 FROM cxthub_fixture_doc_retirement_audit WHERE relation_name IN ('doc_tool_key_members_v1','doc_tool_key_observations_v1')`).Scan(&atomic); err != nil || !atomic {
			test.Fatal("deferred endpoint FKs were not satisfied by one member-before-endpoints transaction", atomic, err)
		}
		removed = true
		break
	}
	if !removed || interiorBatches < 6 {
		test.Fatal("oversized duplicate group did not require multiple real bounded interior batches", removed, interiorBatches)
	}
	docRetirementDrainPG(test, fixture, collector, trace, source, 1024)
	if docRetirementPublicationPG(test, fixture, job, prepared) != before {
		test.Fatal("duplicate-group retirement changed the normally published completed CAS")
	}
	test.Log("normal_tool_keys=144 owned_legacy_duplicate_observations=97 remaining_groups=48")
}

func docRetirementFailureImagePG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image map[string]any
	if err := json.Unmarshal([]byte(docDisplayLifecycleImagePG(test, fixture)), &image); err != nil {
		test.Fatal(err)
	}
	maintenance := image["catalog"].(map[string]any)["display"].(map[string]any)["maintenance"].([]any)
	for _, row := range maintenance {
		delete(row.(map[string]any), "due_at")
	}
	raw, err := json.Marshal(image)
	if err != nil {
		test.Fatal(err)
	}
	return string(raw)
}

func TestDocDisplayRetirementPGForeignGroupEndpointsFailClosed(test *testing.T) {
	output := map[string]any{"middle": true}
	target := domain.HashContent([]byte(`"middle"`))
	for _, prefix := range []string{"before", "zafter"} {
		found := false
		for index := 0; index < 256; index++ {
			key := fmt.Sprintf("%s_%03d", prefix, index)
			raw, err := json.Marshal(key)
			if err != nil {
				test.Fatal(err)
			}
			if domain.HashContent(raw) > target {
				output[key], found = true, true
				break
			}
		}
		if !found {
			test.Fatal("could not construct the bounded lexical endpoint fixture", prefix)
		}
	}
	for _, endpoint := range []string{"first_from", "last_from"} {
		test.Run(endpoint, func(test *testing.T) {
			fixture, job, prepared := docRetirementCompletedPG(test, output, func(fixture docDisplayPGFixture, job domain.DocDisplayJob) {
				group := docRetirementKeyFixturePG(test, fixture, job, endpoint, 0)
				if group.digest != string(target) {
					test.Fatal("malformed fixture must corrupt the first actual PK-selected group", group.digest, target)
				}
			})
			source, _ := prepared.Source.Hash()
			docRetirementAuditPG(test, fixture)
			collector, trace := docRetirementCollectorPG(test, fixture)
			blocker := docRetirementHoldCompletedJobPG(test, fixture, job)
			defer rollbackPG(blocker)
			for turn := 0; turn < 128; turn++ {
				before := docRetirementFailureImagePG(test, fixture)
				step, err := docRetirementStepPG(test, fixture, collector, trace)
				if err == nil {
					if step.Source != source || step.CatalogPhase == "trees" || step.CatalogPhase == "empty" {
						test.Fatal("collector advanced beyond an unverified key-group endpoint", step)
					}
					continue
				}
				if !errors.Is(err, domain.ErrIntegrity) || step != (domain.DocDisplayMaintenanceStep{}) || docRetirementFailureImagePG(test, fixture) != before {
					test.Fatal("malformed endpoint did not fail atomically before further deletion; only due-time deferral is permitted", endpoint, step, err)
				}
				docCatalogProgressGuardsPG(test, fixture)
				return
			}
			test.Fatal("actual collector never rejected its FK-valid foreign-group endpoint", endpoint)
		})
	}
}
