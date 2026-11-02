//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayLifecycleFactPG struct {
	source                      domain.ContentHash
	generation, epoch, jobEpoch string
	checkpoint                  domain.ContentHash
	evidence                    string
}

func readDocDisplayLifecycleFactPG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB, job domain.DocDisplayJob) docDisplayLifecycleFactPG {
	test.Helper()
	var fact docDisplayLifecycleFactPG
	var live bool
	err := database.QueryRow(ctx, `SELECT source.source_hash,admission.generation::text,lifecycle.catalog_epoch::text,
 job.catalog_epoch::text,job.preparation_hash,lifecycle.state='live',jsonb_build_object(
 'source',to_jsonb(source),'admission',to_jsonb(admission),'job',to_jsonb(job),
 'lifecycle',to_jsonb(lifecycle)-'catalog_epoch')::text
 FROM doc_display_jobs_v1 job JOIN doc_display_sources_v1 source ON source.source_hash=job.source_hash
 AND source.event_hash=job.event_hash AND source.profile=job.profile
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash
 AND admission.profile=source.profile AND admission.source_hash=source.source_hash
 JOIN doc_display_catalog_lifecycle_v1 lifecycle ON lifecycle.source_hash=source.source_hash
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.event_index=$3 AND job.profile=$4`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).
		Scan(&fact.source, &fact.generation, &fact.epoch, &fact.jobEpoch, &fact.checkpoint, &live, &fact.evidence)
	use := domain.DocDisplayCatalogUse{Source: fact.source, Generation: fact.generation, Epoch: fact.epoch}
	if err != nil || !live || use.Validate() != nil || domain.ValidateDocDisplayCatalogEpoch(fact.jobEpoch) != nil {
		test.Fatal("fixture lost its actual Source, live lifecycle, or historical checkpoint epoch", fact, err)
	}
	return fact
}

func docDisplayLifecycleImagePG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object('catalog',$1::jsonb,
 'lifecycle',(SELECT jsonb_agg(to_jsonb(lifecycle) ORDER BY source_hash) FROM doc_display_catalog_lifecycle_v1 lifecycle))::text`,
		docCatalogUseImagePG(test, fixture)).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func replaceLiveDocDisplayLifecycleEpochInOwnedTxFixturePG(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, source domain.ContentHash, expectedEpoch string) string {
	test.Helper()
	if _, owned := docDisplayOwnedFixturesPG.Load(fixture.storage.pool.Config().ConnConfig.Database); !owned {
		test.Fatal("deliberate live catalog epoch replacement requires an owned isolated database")
	}
	var owned bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()=$1`, fixture.storage.pool.Config().ConnConfig.Database).Scan(&owned); err != nil || !owned {
		test.Fatal("live epoch replacement transaction must belong to the actual owned database", owned, err)
	}
	const guards = `SELECT count(*) FILTER (WHERE tgname='doc_display_catalog_lifecycle_v1_immutable')=1 AND bool_and(tgenabled='O'),
 jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_catalog_lifecycle_v1'::regclass`
	var enabled bool
	var original, restored, epoch string
	if err := transaction.QueryRow(fixture.ctx, guards).Scan(&enabled, &original); err != nil || !enabled {
		test.Fatal("lifecycle corruption requires the actual enabled immutable guard", enabled, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 DISABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.QueryRow(fixture.ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET catalog_epoch=gen_random_uuid()
 WHERE source_hash=$1 AND catalog_epoch=$2::uuid AND state='live' RETURNING catalog_epoch::text`, source, expectedEpoch).Scan(&epoch); err != nil || epoch == expectedEpoch {
		test.Fatal("fixture did not replace exactly the live catalog epoch", epoch, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 ENABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.QueryRow(fixture.ctx, guards).Scan(&enabled, &restored); err != nil || !enabled || restored != original {
		test.Fatal("lifecycle fixture did not restore every guard before any probe", enabled, err)
	}
	return epoch
}

func advanceLiveDocDisplayLifecycleEpochFixturePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) docDisplayLifecycleFactPG {
	test.Helper()
	before := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
		test.Fatal("own the actual job, affinity, and Source before replacing fixture metadata", err)
	}
	epoch := replaceLiveDocDisplayLifecycleEpochInOwnedTxFixturePG(test, fixture, transaction, before.source, before.epoch)
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	after := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	if after.epoch != epoch || after.evidence != before.evidence || after.generation != before.generation || after.jobEpoch != before.jobEpoch {
		test.Fatal("live epoch replacement changed Source incarnation, checkpoint binding, or other Source/job/lifecycle fields", before, after)
	}
	return after
}

func prepareDocDisplayLifecycleStepPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, beforeOpen func(context.Context)) domain.DocEventDisplayStep {
	test.Helper()
	var step domain.DocEventDisplayStep
	err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		if beforeOpen != nil {
			beforeOpen(read)
		}
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
		if err != nil {
			return err
		}
		reader := work.(*postgresDocEventDisplayWork)
		var tools domain.DocToolPreparationReader
		if prepared.ToolTree != nil && prepared.ToolKeys != nil {
			tools = docCatalogEpochToolsPG(reader, prepared)
			if prepared.ToolOrders != nil {
				tools.OrderPlan = func(read context.Context, parent int) (domain.DocToolOrderPlan, error) {
					return reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders, parent)
				}
			}
		}
		step, err = reader.PrepareStep(read, prepared, docCatalogEpochRawPG(fixture, job), tools)
		return err
	})
	if err != nil || len(step.Objects()) == 0 || step.Prior() != docCatalogUseCheckpointPG(test, prepared) {
		test.Fatal("actual original read scope failed to calculate an advancing epoch-bound step", err)
	}
	return step
}

func bindDocDisplayLifecycleCalculationPG(fixture docDisplayPGFixture, transaction pgx.Tx, job domain.DocDisplayJob, use domain.DocDisplayCatalogUse, epoch, prior any) error {
	_, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid,$8::uuid,$9)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, use.Source, use.Generation, epoch, prior)
	return err
}

func TestDocDisplayLifecyclePGStaleCalculationAndOriginalSnapshot(test *testing.T) {
	for _, timing := range []string{"calculated_before", "snapshot_before"} {
		test.Run(timing, func(test *testing.T) {
			fixture, job, prepared := docCatalogProtocolFixturePG(test)
			original := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
			if original.epoch != original.jobEpoch || original.jobEpoch != docCatalogJobEpochPG(test, fixture, job) {
				test.Fatal("private checkpoint must initially belong to the current live epoch", original)
			}
			var changed docDisplayLifecycleFactPG
			var beforeOpen func(context.Context)
			if timing == "snapshot_before" {
				beforeOpen = func(read context.Context) {
					var generation string
					if err := fixture.storage.db(read).QueryRow(read, `SELECT generation::text FROM doc_display_catalog_admissions_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), original.source).Scan(&generation); err != nil || generation != original.generation {
						test.Fatal("could not establish the original repeatable-read Source snapshot", generation, err)
					}
					changed = advanceLiveDocDisplayLifecycleEpochFixturePG(test, fixture, job)
				}
			}
			step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, beforeOpen)
			use, sealed := step.CatalogUse()
			if !sealed || use.Validate() != nil || use.Source != original.source || use.Generation != original.generation || use.Epoch != original.epoch {
				test.Fatal("original calculation refreshed its captured catalog epoch", use, original)
			}
			if timing == "calculated_before" {
				changed = advanceLiveDocDisplayLifecycleEpochFixturePG(test, fixture, job)
			}
			if changed.epoch == use.Epoch || changed.generation != use.Generation || changed.jobEpoch != use.Epoch || changed.checkpoint != step.Prior() {
				test.Fatal("fixture failed to change only the live epoch while retaining the old checkpoint", changed, use)
			}
			before := docDisplayLifecycleImagePG(test, fixture)
			result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
			if !errors.Is(err, domain.ErrDocDisplayIncarnationChanged) || !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docDisplayLifecycleImagePG(test, fixture) != before {
				test.Fatal("stale catalog epoch save did not fail atomically with the retryable conflict", result, err)
			}
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				fresh := readDocDisplayLifecycleFactPG(test, read, fixture.storage.db(read), job)
				if fresh.epoch != changed.epoch || fresh.jobEpoch != original.epoch {
					test.Fatal("fresh snapshot lost the live/checkpoint epoch disagreement", fresh)
				}
				work, err := fixture.storage.OpenDocEventDisplayWork(read, job, step.Prior())
				if work != nil || !errors.Is(err, domain.ErrDocDisplayIncarnationChanged) {
					test.Fatal("fresh Open relabeled the old private checkpoint", work, err)
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			if docDisplayLifecycleImagePG(test, fixture) != before {
				test.Fatal("failed fresh Open changed the old checkpoint or catalogs")
			}
			reset, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
			initial, initialErr := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{DocEventIndex: job.Target.Event, Kind: prepared.Source.Kind})
			if err != nil || initialErr != nil || !reflect.DeepEqual(reset, initial) || docCatalogUseCheckpointPG(test, reset) == step.Prior() {
				test.Fatal("normal initialization did not replace the stale checkpoint with exact canonical initial state", reset, err, initialErr)
			}
			current := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
			if current.epoch != changed.epoch || current.jobEpoch != changed.epoch || current.generation != original.generation || current.checkpoint != docCatalogUseCheckpointPG(test, reset) {
				test.Fatal("canonical reset lost the new checkpoint epoch or changed Source incarnation", current)
			}
			fresh := prepareDocDisplayLifecycleStepPG(test, fixture, job, reset, nil)
			freshUse, sealed := fresh.CatalogUse()
			if !sealed || freshUse.Epoch != changed.epoch || freshUse.Generation != original.generation || freshUse.Source != original.source {
				test.Fatal("fresh recalculation did not capture its own actual epoch", freshUse)
			}
			saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, fresh)
			if err != nil || !reflect.DeepEqual(saved, fresh.Preparation()) {
				test.Fatal("actual fresh recalculation remained unusable after canonical reset", err)
			}
		})
	}
}

func TestDocDisplayLifecyclePGLegacyCalculationCannotRefreshEpoch(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	use, sealed := step.CatalogUse()
	if !sealed {
		test.Fatal("actual private calculation lacks its captured use")
	}
	advanceLiveDocDisplayLifecycleEpochFixturePG(test, fixture, job)
	before := docDisplayLifecycleImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	_, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, use.Source, use.Generation)
	docCatalogProtocolDeniedPG(test, err, "0A000", "document catalog calculations require writer protocol 3")
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if docDisplayLifecycleImagePG(test, fixture) != before {
		test.Fatal("legacy calculation changed the live epoch or checkpoint binding")
	}
}

func TestDocDisplayLifecyclePGCalculationBindsEpochAndPredecessor(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	use, sealed := step.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("actual private calculation lacks a valid epoch-bound use", use)
	}
	foreignEpoch := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if foreignEpoch == use.Epoch {
		test.Fatal("foreign epoch unexpectedly equals the fixture epoch")
	}
	before := docDisplayLifecycleImagePG(test, fixture)
	for _, selected := range []struct {
		name          string
		epoch, prior  any
		code, message string
	}{
		{"missing_epoch", nil, step.Prior(), "0A000", "document catalog calculation binding is invalid"},
		{"zero_epoch", "00000000-0000-0000-0000-000000000000", step.Prior(), "0A000", "document catalog calculation binding is invalid"},
		{"wrong_epoch", foreignEpoch, step.Prior(), "55000", "document catalog epoch changed"},
		{"missing_prior", use.Epoch, nil, "0A000", "document catalog calculation binding is invalid"},
		{"invalid_prior", use.Epoch, "invalid", "0A000", "document catalog calculation binding is invalid"},
		{"wrong_prior", use.Epoch, domain.HashContent([]byte("foreign catalog predecessor")), "55000", "document catalog writer claim is not live"},
	} {
		test.Run(selected.name, func(test *testing.T) {
			transaction := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(transaction)
			err := bindDocDisplayLifecycleCalculationPG(fixture, transaction, job, use, selected.epoch, selected.prior)
			docCatalogProtocolDeniedPG(test, err, selected.code, selected.message)
			if err := transaction.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if docDisplayLifecycleImagePG(test, fixture) != before {
				test.Fatal("invalid calculation binding changed durable lifecycle or work", selected.name)
			}
		})
	}
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if err := bindDocDisplayLifecycleCalculationPG(fixture, transaction, job, use, use.Epoch, step.Prior()); err != nil {
		test.Fatal("current epoch and predecessor must authorize actual private work", err)
	}
	scope, err := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
	if err != nil {
		test.Fatal(err)
	}
	if tag, err := transaction.Exec(fixture.ctx, `UPDATE doc_tool_order_plans_v1 SET plan_hash=plan_hash
 WHERE scope=$1 AND parent_from=$2`, scope, prepared.ToolTree.Root.From); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("current calculation could not replay the actual guarded private row", tag, err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil || docDisplayLifecycleImagePG(test, fixture) != before {
		test.Fatal("authorized exact private replay changed persistent state", err)
	}
	saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil || !reflect.DeepEqual(saved, step.Preparation()) {
		test.Fatal("current actual sealed step did not remain usable through Save", err)
	}
}

func TestDocDisplayLifecyclePGEpochOnlyProgressAndRetirementStayGuarded(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	use, sealed := step.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("epoch guard fixture lacks an actual valid calculation", use)
	}
	foreignEpoch := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	if foreignEpoch == use.Epoch {
		test.Fatal("foreign epoch unexpectedly equals the fixture epoch")
	}
	before := docDisplayLifecycleImagePG(test, fixture)
	for _, mode := range []string{"control", "calculation", "inactive", "retirement"} {
		test.Run(mode, func(test *testing.T) {
			transaction := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(transaction)
			var err error
			if mode == "calculation" || mode == "retirement" {
				err = bindDocDisplayLifecycleCalculationPG(fixture, transaction, job, use, use.Epoch, step.Prior())
			} else {
				err = docCatalogProtocolControlPG(fixture, transaction, job)
			}
			if err != nil {
				test.Fatal("acquire actual authority before the raw epoch mutation", err)
			}
			message, guard := "document catalog writes require a bound calculation", "cxthub_guard_doc_catalog_job_write"
			if mode == "inactive" {
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET state='waiting',lease_until=NULL
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index)
				message = "inactive document display jobs cannot change progress"
			} else if mode == "calculation" {
				message = "document catalog calculation binding is invalid"
			}
			if mode == "retirement" {
				_, err = transaction.Exec(fixture.ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET state='retiring' WHERE source_hash=$1`, use.Source)
				message, guard = "document catalog lifecycle is immutable", "cxthub_guard_doc_catalog_lifecycle"
			} else {
				_, err = transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET catalog_epoch=$4::uuid
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index, foreignEpoch)
			}
			docCatalogProtocolDeniedPG(test, err, "0A000", message)
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || !strings.Contains(failure.Where, guard) {
				test.Fatal("raw epoch mutation failed outside its intended actual guard", mode, err)
			}
			if err := transaction.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if docDisplayLifecycleImagePG(test, fixture) != before {
				test.Fatal("rejected raw epoch mutation changed lifecycle, checkpoint, or catalogs", mode)
			}
		})
	}
}

func TestDocDisplayLifecyclePGPrivateReplaysRecheckEpochAfterBinding(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	use, sealed := step.CatalogUse()
	original := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	if !sealed || use.Validate() != nil || original.epoch != use.Epoch || original.jobEpoch != use.Epoch || original.checkpoint != step.Prior() {
		test.Fatal("private replay fixture lacks its original sealed calculation and checkpoint", use, original)
	}
	before := docDisplayLifecycleImagePG(test, fixture)
	for _, table := range docCatalogWriterTablesPG {
		for _, replay := range docCatalogProtocolReplaysPG(table) {
			test.Run(table+"/"+replay.operation, func(test *testing.T) {
				transaction := docCatalogUseBeginPG(test, fixture)
				defer rollbackPG(transaction)
				if err := bindDocDisplayLifecycleCalculationPG(fixture, transaction, job, use, use.Epoch, step.Prior()); err != nil {
					test.Fatal("bind the actual captured calculation before replacing the live epoch", err)
				}
				var binding, retained string
				if err := transaction.QueryRow(fixture.ctx, `SELECT current_setting('cxthub.doc_catalog_use',true)`).Scan(&binding); err != nil || binding == "" {
					test.Fatal("actual calculation binder did not install its captured proof", err)
				}
				epoch := replaceLiveDocDisplayLifecycleEpochInOwnedTxFixturePG(test, fixture, transaction, use.Source, use.Epoch)
				changed := readDocDisplayLifecycleFactPG(test, fixture.ctx, transaction, job)
				if changed.epoch != epoch || changed.evidence != original.evidence || changed.generation != use.Generation || changed.jobEpoch != use.Epoch || changed.checkpoint != step.Prior() {
					test.Fatal("same-transaction mutation changed more than the live lifecycle epoch", original, changed)
				}
				if err := transaction.QueryRow(fixture.ctx, `SELECT current_setting('cxthub.doc_catalog_use',true)`).Scan(&retained); err != nil || retained != binding {
					test.Fatal("fixture cleared or refreshed the already-bound calculation", err)
				}
				_, err := transaction.Exec(fixture.ctx, replay.statement)
				docCatalogProtocolDeniedPG(test, err, "55000", "document catalog epoch changed")
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || !strings.Contains(failure.Where, "cxthub_guard_doc_tool_catalog_write") || !strings.Contains(failure.Where, "cxthub_require_doc_catalog_writer") {
					test.Fatal("private replay failed outside the actual epoch-rechecking writer guard", table, replay.operation, err)
				}
				if err := transaction.Rollback(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				if docDisplayLifecycleImagePG(test, fixture) != before {
					test.Fatal("failed stale private replay changed lifecycle, Source, jobs, objects, or catalogs", table, replay.operation)
				}
			})
		}
	}
}
