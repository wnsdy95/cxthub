//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docCatalogEpochRawPG(fixture docDisplayPGFixture, job domain.DocDisplayJob) func(context.Context, int, int) ([]byte, error) {
	return func(read context.Context, from, length int) ([]byte, error) {
		if err := read.Err(); err != nil {
			return nil, err
		}
		if from < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || from > job.Target.Event.Length-length {
			return nil, domain.ErrIntegrity
		}
		start := job.Target.Event.Offset + from
		return bytes.Clone(fixture.stream[start : start+length]), nil
	}
}

func docCatalogEpochToolsPG(reader *postgresDocEventDisplayWork, prepared domain.DocEventDisplayPreparation) domain.DocToolPreparationReader {
	tree, keys := *prepared.ToolTree, *prepared.ToolKeys
	return domain.DocToolPreparationReader{
		Nodes: func(read context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			return reader.ReadToolTreeNodes(read, tree, after, limit)
		},
		OrderSource: func(read context.Context, parent int) (domain.DocToolOrderSource, error) {
			return reader.ReadToolOrderSource(read, tree, keys, parent)
		},
		OrderEntries: func(read context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
			return reader.ReadToolOrderEntries(read, tree, keys, source, after, limit)
		},
	}
}

func docCatalogEpochRequireFailurePG(test *testing.T, label string, result domain.DocEventDisplayStep, failure, expected error) {
	test.Helper()
	if failure == nil || expected != nil && !errors.Is(failure, expected) || !reflect.DeepEqual(result, domain.DocEventDisplayStep{}) {
		test.Fatal(label, "must return an error and a zero, unstamped step", failure, expected)
	}
	if errors.Is(expected, domain.ErrConflict) && errors.Is(failure, domain.ErrDocDisplayIncarnationChanged) {
		test.Fatal(label, "was misclassified as a retryable incarnation change", failure)
	}
}

func docCatalogEpochABAPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, sourceRaw []byte) docCatalogAdmissionPGFact {
	test.Helper()
	sourceHash := domain.HashContent(sourceRaw)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	before := docCatalogUseImagePG(test, fixture)
	current := admission
	for _, replacement := range [][]byte{[]byte(`{}`), bytes.Clone(sourceRaw)} {
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET bytes=$4
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), sourceHash, replacement)
		if err := transaction.Commit(fixture.ctx); err != nil {
			test.Fatal("commit same-source ABA transition", err)
		}
		next := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
		if next.source != admission.source || next.revision != admission.revision || next.generation == current.generation {
			test.Fatal("committed source-byte transition did not advance only its real admission generation", current, next)
		}
		current = next
	}
	if current.generation == admission.generation || strings.Count(before, admission.generation) != 1 {
		test.Fatal("same-source ABA did not replace exactly one original admission generation", admission, current)
	}
	if expected := strings.Replace(before, admission.generation, current.generation, 1); docCatalogUseImagePG(test, fixture) != expected {
		test.Fatal("same-source ABA changed source identity, canonical bytes, job, or catalog data beyond its generation")
	}
	return current
}

func TestDocDisplayCatalogEpochPGSameSourceABARejectsCalculatedStep(test *testing.T) {
	isolated, _ := rootProjectionIsolatedPG(test)
	test.Setenv("CXT_TEST_DSN", isolated.pool.Config().ConnString())
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"first": true, "second": "value"})
	fixture = docCatalogUseBoundPG(test, fixture)
	if fixture.storage.pool.Config().ConnConfig.Database != isolated.pool.Config().ConnConfig.Database ||
		prepared.Phase != "tool_orders" || prepared.ToolTree == nil || prepared.ToolKeys == nil || prepared.ToolOrders == nil {
		test.Fatal("fixture did not retain its isolated private preparation", prepared.Phase)
	}
	canonicalSource, err := prepared.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	sourceHash := domain.HashContent(canonicalSource)
	var storedSource domain.ContentHash
	var sourceRaw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash,bytes FROM doc_display_sources_v1
 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile()).Scan(&storedSource, &sourceRaw); err != nil ||
		storedSource != sourceHash || !bytes.Equal(sourceRaw, canonicalSource) {
		test.Fatal("fixture source was not the exact canonical source", storedSource, err)
	}
	var version int64
	var checkpoint, jobSource domain.ContentHash
	var preparationRaw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT version,preparation_hash,source_hash,preparation
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).
		Scan(&version, &checkpoint, &jobSource, &preparationRaw); err != nil || version != job.Version || jobSource != sourceHash ||
		checkpoint != docCatalogUseCheckpointPG(test, prepared) || domain.HashContent(preparationRaw) != checkpoint {
		test.Fatal("fixture job did not retain its actual version, checkpoint, and source", version, checkpoint, jobSource, err)
	}
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	if admission.source != string(sourceHash) {
		test.Fatal("fixture admission did not bind the canonical source", admission)
	}
	before := docCatalogUseImagePG(test, fixture)
	calculated := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	if calculated.Prior() != checkpoint || len(calculated.Objects()) == 0 ||
		docCatalogUseCheckpointPG(test, calculated.Preparation()) == checkpoint {
		test.Fatal("actual read snapshot did not calculate an advancing sealed step")
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("read-only calculation changed durable state")
	}
	use, sealed := calculated.CatalogUse()
	if !sealed || use.Source != sourceHash || use.Generation != admission.generation {
		test.Fatal("actual preparation did not capture its source and admission generation", use, sealed)
	}
	current := docCatalogEpochABAPG(test, fixture, job, sourceRaw)
	afterABA := docCatalogUseImagePG(test, fixture)
	rebound, rebindErr := calculated.WithCatalogUse(domain.DocDisplayCatalogUse{Source: sourceHash, Generation: current.generation, Epoch: use.Epoch})
	docCatalogEpochRequireFailurePG(test, "rebinding an old bound step to a new generation", rebound, rebindErr, domain.ErrConflict)
	result, saveErr := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, calculated)
	rejected := errors.Is(saveErr, domain.ErrConflict) && errors.Is(saveErr, domain.ErrDocDisplayIncarnationChanged)
	if !rejected {
		test.Errorf("step calculated before a committed same-source ABA must fail with ErrConflict; got %v", saveErr)
	}
	empty := reflect.DeepEqual(result, domain.DocEventDisplayPreparation{})
	if !empty {
		test.Error("stale-generation save returned a preparation instead of an empty conflict result")
	}
	unchanged := docCatalogUseImagePG(test, fixture) == afterABA
	if !unchanged {
		test.Error("stale-generation save changed durable job, objects, or catalog data")
	}
	if !rejected || !empty || !unchanged {
		return
	}
	fresh := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	if freshUse, sealed := fresh.CatalogUse(); !sealed || freshUse.Source != sourceHash || freshUse.Generation != current.generation {
		test.Fatal("fresh calculation did not capture the replacement generation", freshUse, sealed)
	}
	result, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, fresh)
	if err != nil || !reflect.DeepEqual(result, fresh.Preparation()) || docCatalogUseCheckpointPG(test, result) == checkpoint {
		test.Fatal("fresh calculation after the generation transition must remain usable", err)
	}
}

func TestDocDisplayCatalogEpochPGPrepareStepScopeAndMissingSeal(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	foreign, err := NewPostgresStore(fixture.ctx, fixture.storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(foreign.Close)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	before := docCatalogUseImagePG(test, fixture)
	loadRaw := docCatalogEpochRawPG(fixture, job)
	var calculated domain.DocEventDisplayStep
	var escaped *postgresDocEventDisplayWork
	var original context.Context
	err = fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
		if err != nil {
			return err
		}
		reader := work.(*postgresDocEventDisplayWork)
		escaped, original = reader, read
		tools := docCatalogEpochToolsPG(reader, prepared)
		unsealed, err := domain.StepDocEventDisplay(read, prepared, loadRaw, reader.LoadObject, tools)
		if err != nil {
			return err
		}
		if _, sealed := unsealed.CatalogUse(); sealed {
			test.Fatal("pure calculation unexpectedly had storage admission authority")
		}
		result, saveErr := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, unsealed)
		if !errors.Is(saveErr, domain.ErrConflict) || errors.Is(saveErr, domain.ErrDocDisplayIncarnationChanged) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docCatalogUseImagePG(test, fixture) != before {
			test.Fatal("missing-seal save must conflict without changing durable state", saveErr)
		}
		wrong := prepared
		wrong.Active = domain.HashContent([]byte("another predecessor checkpoint"))
		step, failure := reader.PrepareStep(read, wrong, loadRaw, tools)
		docCatalogEpochRequireFailurePG(test, "wrong predecessor", step, failure, domain.ErrConflict)
		wrong = prepared
		wrong.Source.Kind = domain.EventToolCall
		step, failure = reader.PrepareStep(read, wrong, loadRaw, tools)
		docCatalogEpochRequireFailurePG(test, "wrong source", step, failure, domain.ErrConflict)
		wrong = prepared
		wrong.Phase = "invalid"
		step, failure = reader.PrepareStep(read, wrong, loadRaw, tools)
		docCatalogEpochRequireFailurePG(test, "invalid predecessor state", step, failure, nil)
		var absent *postgresDocEventDisplayWork
		step, failure = absent.PrepareStep(read, prepared, loadRaw, tools)
		docCatalogEpochRequireFailurePG(test, "nil work reader", step, failure, domain.ErrConflict)
		withoutUse := *reader
		withoutUse.use = domain.DocDisplayCatalogUse{}
		step, failure = withoutUse.PrepareStep(read, prepared, loadRaw, tools)
		docCatalogEpochRequireFailurePG(test, "missing captured use", step, failure, nil)
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			step, failure := reader.PrepareStep(other, prepared, loadRaw, tools)
			docCatalogEpochRequireFailurePG(test, "another read transaction", step, failure, domain.ErrConflict)
			return nil
		}); err != nil {
			return err
		}
		if err := foreign.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			step, failure := reader.PrepareStep(other, prepared, loadRaw, tools)
			docCatalogEpochRequireFailurePG(test, "another store's read transaction", step, failure, domain.ErrConflict)
			return nil
		}); err != nil {
			return err
		}
		calculated, err = reader.PrepareStep(read, prepared, loadRaw, tools)
		if err != nil {
			return err
		}
		use, sealed := calculated.CatalogUse()
		if !sealed || use.Source != domain.ContentHash(admission.source) || use.Generation != admission.generation ||
			calculated.Prior() != unsealed.Prior() || !reflect.DeepEqual(calculated.Preparation(), unsealed.Preparation()) {
			test.Fatal("actual scoped preparation did not seal the same captured predecessor and source", use, sealed)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	step, failure := escaped.PrepareStep(original, prepared, loadRaw, docCatalogEpochToolsPG(escaped, prepared))
	docCatalogEpochRequireFailurePG(test, "original context after snapshot commit", step, failure, nil)
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("scope checks or scoped preparation changed durable state")
	}
	result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, calculated)
	if err != nil || !reflect.DeepEqual(result, calculated.Preparation()) {
		test.Fatal("properly scoped preparation was unusable after its read snapshot ended", err)
	}
}

func TestDocDisplayCatalogEpochPGPrepareStepTransactionLifetime(test *testing.T) {
	for _, ending := range []string{"canceled", "aborted", "closed"} {
		test.Run(ending, func(test *testing.T) {
			fixture, job, prepared := docCatalogUseFixturePG(test)
			before := docCatalogUseImagePG(test, fixture)
			reading, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			abort := errors.New("end interrupted preparation snapshot")
			var escaped *postgresDocEventDisplayWork
			var original context.Context
			loadRaw := docCatalogEpochRawPG(fixture, job)
			err := fixture.storage.WithinReadSnapshot(reading, func(read context.Context) error {
				work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
				if err != nil {
					return err
				}
				reader := work.(*postgresDocEventDisplayWork)
				escaped, original = reader, read
				tools := docCatalogEpochToolsPG(reader, prepared)
				if ending == "canceled" {
					canceled, stop := context.WithCancel(read)
					stop()
					step, failure := reader.PrepareStep(canceled, prepared, loadRaw, tools)
					docCatalogEpochRequireFailurePG(test, "already canceled preparation", step, failure, context.Canceled)
				}
				entries := tools.OrderEntries
				interrupted := false
				tools.OrderEntries = func(bound context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
					result, err := entries(bound, source, after, limit)
					if err != nil {
						return nil, err
					}
					interrupted = true
					switch ending {
					case "canceled":
						cancel()
					case "aborted":
						if _, err := reader.owner.Exec(bound, `SELECT 1/0`); err == nil || reader.owner.Conn().PgConn().TxStatus() != 'E' {
							test.Fatal("actual read transaction did not enter the aborted state", err)
						}
					case "closed":
						if err := reader.owner.Rollback(bound); err != nil {
							test.Fatal("close original read transaction during calculation", err)
						}
					}
					return result, nil
				}
				step, failure := reader.PrepareStep(read, prepared, loadRaw, tools)
				if !interrupted {
					test.Fatal("preparation did not reach the actual private catalog read")
				}
				var expected error
				if ending == "canceled" {
					expected = context.Canceled
				}
				docCatalogEpochRequireFailurePG(test, "transaction ended during actual preparation", step, failure, expected)
				step, failure = reader.PrepareStep(read, prepared, loadRaw, docCatalogEpochToolsPG(reader, prepared))
				docCatalogEpochRequireFailurePG(test, "transaction already ended before preparation", step, failure, expected)
				return abort
			})
			if !errors.Is(err, abort) {
				test.Fatal("unexpected interrupted snapshot completion", err)
			}
			step, failure := escaped.PrepareStep(original, prepared, loadRaw, docCatalogEpochToolsPG(escaped, prepared))
			docCatalogEpochRequireFailurePG(test, "escaped original transaction context", step, failure, nil)
			docCatalogUseProbePG(test, fixture, job.Target, true)
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("failed preparation changed durable job, objects, or catalog data")
			}
		})
	}
}

func TestDocDisplayCatalogEpochPGOldSnapshotCapturesOldGeneration(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	sourceRaw, err := prepared.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var calculated domain.DocEventDisplayStep
	var current docCatalogAdmissionPGFact
	err = fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		observed := docCatalogAdmissionFactPG(test, read, fixture.storage.db(read), job.Target.Event.Hash, job.Target.DisplayProfile())
		if observed != admission {
			test.Fatal("old repeatable-read snapshot did not observe the original admission", observed, admission)
		}
		current = docCatalogEpochABAPG(test, fixture, job, sourceRaw)
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
		if err != nil {
			return err
		}
		reader := work.(*postgresDocEventDisplayWork)
		calculated, err = reader.PrepareStep(read, prepared, docCatalogEpochRawPG(fixture, job), docCatalogEpochToolsPG(reader, prepared))
		if err != nil {
			return err
		}
		use, sealed := calculated.CatalogUse()
		if !sealed || use.Source != domain.HashContent(sourceRaw) || use.Generation != admission.generation || use.Generation == current.generation {
			test.Fatal("old snapshot refreshed or relabeled its captured admission generation", use, admission, current)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	afterABA := docCatalogUseImagePG(test, fixture)
	if fixture.storage.InTransaction(fixture.ctx) {
		test.Fatal("stale-snapshot save must execute in an actual new write transaction")
	}
	result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, calculated)
	if !errors.Is(err, domain.ErrConflict) || !errors.Is(err, domain.ErrDocDisplayIncarnationChanged) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docCatalogUseImagePG(test, fixture) != afterABA {
		test.Fatal("new write transaction accepted an old-snapshot generation or mutated durable state", err)
	}
	fresh := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	if use, sealed := fresh.CatalogUse(); !sealed || use.Generation != current.generation {
		test.Fatal("fresh snapshot did not capture the current admission generation", use, sealed)
	}
	result, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, fresh)
	if err != nil || !reflect.DeepEqual(result, fresh.Preparation()) {
		test.Fatal("fresh recomputation after an old-snapshot conflict must remain usable", err)
	}
}

func TestDocDisplayCatalogEpochPGInvalidAdmissionDoesNotPin(test *testing.T) {
	for _, failure := range []string{"missing", "mismatching_source", "zero_uuid"} {
		test.Run(failure, func(test *testing.T) {
			fixture, job, prepared := docCatalogUseFixturePG(test)
			switch failure {
			case "missing":
				docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `DELETE FROM doc_display_catalog_admissions_v1
 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile())
			case "mismatching_source":
				foreignSource, _ := maintenanceSource(test, fixture.ctx, fixture.storage, "actual foreign catalog source")
				docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_catalog_admissions_v1 SET source_hash=$3
 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile(), foreignSource)
			case "zero_uuid":
				docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool, `UPDATE doc_display_catalog_admissions_v1
 SET generation='00000000-0000-0000-0000-000000000000'::uuid WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile())
			}
			before := docCatalogUseImagePG(test, fixture)
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
				if !errors.Is(err, domain.ErrIntegrity) || reader != nil {
					test.Fatal("invalid admission returned data or weakened source authority", reader, err)
				}
				docCatalogUseProbePG(test, fixture, job.Target, true)
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			docCatalogUseProbePG(test, fixture, job.Target, true)
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("invalid admission open changed durable state")
			}
		})
	}
}
