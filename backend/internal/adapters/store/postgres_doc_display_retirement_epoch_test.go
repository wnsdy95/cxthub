//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docDisplayRetirementEpochFixturePG(test *testing.T) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	rootProjectionIsolatedPG(test)
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"first": []any{1, 2}, "second": map[string]any{"value": true}})
	for count := 0; count < 128 && prepared.Phase != "completed"; count++ {
		var step domain.DocEventDisplayStep
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
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
		}); err != nil {
			test.Fatal("calculate actual completed tool projection", prepared.Phase, err)
		}
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("save actual completed tool projection", err)
		}
	}
	if prepared.Phase != "completed" {
		test.Fatal("bounded tool fixture did not complete", prepared.Phase)
	}
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, docCatalogUseCheckpointPG(test, prepared), time.Now()); err != nil {
		test.Fatal(err)
	}
	return fixture, job, prepared
}

func TestDocDisplayRetirementPGFreshEpochRequiresCanonicalRebuild(test *testing.T) {
	fixture, job, completed := docDisplayRetirementEpochFixturePG(test)
	original := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	drainDisplayMaintenance(test, fixture.ctx, fixture.storage)
	var state, retiredEpoch string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,catalog_epoch::text FROM doc_display_catalog_lifecycle_v1
 WHERE source_hash=$1`, original.source).Scan(&state, &retiredEpoch); err != nil || state != "retired" || retiredEpoch != original.epoch {
		test.Fatal("actual cleanup did not retire the first epoch", state, retiredEpoch, err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index); err != nil {
		test.Fatal("remove the completed receipt, not the preserved public CAS", err)
	}
	receiver := docDisplayPG(test, fixture.doc.cir.Events...)
	enqueueDocEventDisplayQueue(test, receiver, 0)
	claim := receiver.claim(test)
	beforeRestart := docDisplayLifecycleImagePG(test, receiver)
	invalid := docCatalogUseBeginPG(test, receiver)
	if err := docCatalogProtocolControlPG(receiver, invalid, claim); err != nil {
		test.Fatal(err)
	}
	if _, err := invalid.Exec(receiver.ctx, `SELECT set_config('cxthub.doc_catalog_restart',$1,true)`, string(original.source)+":"+original.epoch); err != nil {
		test.Fatal(err)
	}
	if _, err := invalid.Exec(receiver.ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET state='live',catalog_epoch=gen_random_uuid(),
 retire_generation=NULL,retire_phase='',retire_scope='' WHERE source_hash=$1`, original.source); err != nil {
		test.Fatal("negative fixture must reach the deferred atomic-initialization check", err)
	}
	var rejected *pgconn.PgError
	if err := invalid.Commit(receiver.ctx); !errors.As(err, &rejected) || rejected.Code != "23514" || !strings.Contains(rejected.Where, "cxthub_verify_doc_catalog_restart") {
		test.Fatal("a real writer with only a forged restart flag could commit an uninitialized epoch", err)
	}
	if docDisplayLifecycleImagePG(test, receiver) != beforeRestart {
		test.Fatal("failed raw restart changed the durable retired epoch or job")
	}
	initial, err := receiver.storage.LoadDocEventDisplay(receiver.ctx, claim)
	if err != nil || initial.Source != completed.Source || initial.Phase != "tool_discovery" || initial.ToolTree != nil || initial.ToolKeys != nil || initial.ToolOrders != nil {
		test.Fatal("fresh epoch must start at the canonical preparation, never an old partial catalog", initial.Phase, err)
	}
	fresh := readDocDisplayLifecycleFactPG(test, receiver.ctx, receiver.storage.pool, claim)
	if fresh.epoch == original.epoch || fresh.jobEpoch != fresh.epoch || fresh.generation != original.generation || fresh.source != original.source {
		test.Fatal("fresh epoch changed content identity or reused the retired epoch", original, fresh)
	}
	transaction := docCatalogUseBeginPG(test, receiver)
	before := docDisplayLifecycleImagePG(test, receiver)
	err = bindDocDisplayLifecycleCalculationPG(receiver, transaction, claim,
		domain.DocDisplayCatalogUse{Source: original.source, Generation: original.generation, Epoch: original.epoch}, original.epoch, fresh.checkpoint)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "55000" || failure.Message != "document catalog epoch changed" {
		test.Fatal("retired epoch with the exact current predecessor did not fail at epoch comparison", err)
	}
	rollbackPG(transaction)
	if docDisplayLifecycleImagePG(test, receiver) != before {
		test.Fatal("retired calculation changed the new epoch")
	}
	var step domain.DocEventDisplayStep
	if err := receiver.storage.WithinReadSnapshot(receiver.ctx, func(read context.Context) error {
		work, err := receiver.storage.OpenDocEventDisplayWork(read, claim, fresh.checkpoint)
		if err != nil {
			return err
		}
		step, err = work.PrepareStep(read, initial, docCatalogEpochRawPG(receiver, claim), domain.DocToolPreparationReader{})
		return err
	}); err != nil {
		test.Fatal("fresh original read could not calculate from retained input", err)
	}
	if use, sealed := step.CatalogUse(); !sealed || use.Source != fresh.source || use.Generation != fresh.generation || use.Epoch != fresh.epoch {
		test.Fatal("fresh calculation did not carry its actual new epoch", use, sealed)
	}
	if saved, err := receiver.storage.SaveDocEventDisplay(receiver.ctx, claim, step); err != nil || !reflect.DeepEqual(saved, step.Preparation()) {
		test.Fatal("fresh canonical preparation could not resume without administrator intervention", err)
	}
	var private bool
	prepared := step.Preparation()
	for count := 0; count < 4; count++ {
		if err := receiver.storage.pool.QueryRow(receiver.ctx, `SELECT EXISTS(SELECT 1 FROM doc_tool_tree_sources_v1 WHERE source_hash=$1)`, fresh.source).Scan(&private); err != nil {
			test.Fatal(err)
		}
		if private {
			break
		}
		next := prepareDocDisplayLifecycleStepPG(test, receiver, claim, prepared, nil)
		prepared, err = receiver.storage.SaveDocEventDisplay(receiver.ctx, claim, next)
		if err != nil {
			test.Fatal("rebuild actual second-epoch private rows", err)
		}
	}
	if !private {
		test.Fatal("new epoch must actually reconstruct its own private catalog before retirement")
	}
	if _, err := receiver.storage.pool.Exec(receiver.ctx, `DELETE FROM doc_display_jobs_v1 WHERE `+docDisplayKey(claim.Target), claim.RepoID, claim.DocHash, claim.Target.Event.Index); err != nil {
		test.Fatal(err)
	}
	drainDisplayMaintenance(test, receiver.ctx, receiver.storage)
	var secondState, secondEpoch string
	if err := receiver.storage.pool.QueryRow(receiver.ctx, `SELECT state,catalog_epoch::text FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$1`, fresh.source).Scan(&secondState, &secondEpoch); err != nil || secondState != "retired" || secondEpoch != fresh.epoch {
		test.Fatal("the second actual catalog epoch did not retire", secondState, secondEpoch, err)
	}
	third := docDisplayPG(test, fixture.doc.cir.Events...)
	enqueueDocEventDisplayQueue(test, third, 0)
	thirdClaim := third.claim(test)
	rewind := docCatalogUseBeginPG(test, third)
	if err := docCatalogProtocolControlPG(third, rewind, thirdClaim); err != nil {
		test.Fatal(err)
	}
	if _, err := rewind.Exec(third.ctx, `SELECT set_config('cxthub.doc_catalog_restart',$1,true)`, string(fresh.source)+":"+fresh.epoch); err != nil {
		test.Fatal(err)
	}
	var actual string
	if err := rewind.QueryRow(third.ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET state='live',catalog_epoch=$2::uuid,
 retire_generation=NULL,retire_phase='',retire_scope='' WHERE source_hash=$1 RETURNING catalog_epoch::text`, fresh.source, original.epoch).Scan(&actual); err != nil || actual == original.epoch || actual == fresh.epoch || domain.ValidateDocDisplayCatalogEpoch(actual) != nil {
		test.Fatal("a real writer could rewind E2 to the caller-selected E1", actual, err)
	}
	rollbackPG(rewind)
}
