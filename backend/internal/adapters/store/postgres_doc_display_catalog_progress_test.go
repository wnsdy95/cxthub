//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docCatalogProgressGuardsPG(test *testing.T, fixture docDisplayPGFixture) {
	test.Helper()
	docCatalogProgressGuardsOnPG(test, fixture, fixture.storage.pool)
}

func docCatalogProgressGuardsOnPG(test *testing.T, fixture docDisplayPGFixture, database docCatalogAdmissionPGDB) {
	test.Helper()
	if _, owned := docDisplayOwnedFixturesPG.Load(fixture.storage.pool.Config().ConnConfig.Database); !owned {
		test.Fatal("catalog progress tests require an owned isolated database")
	}
	var enabled bool
	if err := database.QueryRow(fixture.ctx, `SELECT bool_and(tgenabled='O')
 AND count(*) FILTER(WHERE tgrelid='doc_display_jobs_v1'::regclass AND tgname='guard_doc_display_zz_catalog_write')=1
 FROM pg_trigger WHERE NOT tgisinternal AND tgrelid=ANY(ARRAY[
 'doc_display_jobs_v1'::regclass,'doc_display_sources_v1'::regclass,'doc_display_catalog_admissions_v1'::regclass,
 'doc_tool_tree_sources_v1'::regclass,'doc_tool_tree_nodes_v1'::regclass,'doc_tool_key_sources_v1'::regclass,
 'doc_tool_key_observations_v1'::regclass,'doc_tool_key_members_v1'::regclass,
 'doc_tool_order_sources_v1'::regclass,'doc_tool_order_plans_v1'::regclass])`).Scan(&enabled); err != nil || !enabled {
		test.Fatal("catalog progress tests require the actual enabled job, Source, admission, and private guards", enabled, err)
	}
}

func docCatalogProgressStartPG(test *testing.T, kind domain.EventKind) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	rootProjectionIsolatedPG(test)
	event := domain.CIREvent{Kind: kind, Seq: 1}
	if kind == domain.EventReasoning {
		event.RedactedSummary = "catalog progress " + test.Name()
	} else {
		event.CallID, event.Output = test.Name(), "catalog progress"
	}
	fixture := docDisplayPG(test, event)
	enqueueDocEventDisplayQueue(test, fixture, 0)
	job := fixture.claim(test)
	docCatalogProgressGuardsPG(test, fixture)
	var initial domain.DocEventDisplayPreparation
	err := fixture.storage.docDisplayWriteEntry(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx, entry domain.DocRangeEntry) error {
		var err error
		initial, err = domain.NewDocEventDisplayPreparation(entry)
		if err != nil {
			return err
		}
		raw, err := initial.Source.Encode()
		if err != nil {
			return err
		}
		_, err = transaction.Exec(bound, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, initial.Source.EventHash, initial.Source.Profile, domain.HashContent(raw), raw)
		return err
	})
	if err != nil {
		test.Fatal("create the actual canonical Source under live control without attaching progress", err)
	}
	var unbound bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source_hash IS NULL AND preparation_hash IS NULL AND preparation IS NULL
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&unbound); err != nil || !unbound {
		test.Fatal("initialization fixture already contains progress", unbound, err)
	}
	return fixture, job, initial
}

func docCatalogProgressLoadPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) domain.DocEventDisplayPreparation {
	test.Helper()
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	return prepared
}

func docCatalogProgressWritePG(fixture docDisplayPGFixture, transaction pgx.Tx, job domain.DocDisplayJob, source domain.ContentHash, raw []byte) (pgconn.CommandTag, error) {
	return transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET source_hash=$4,preparation_hash=$5,preparation=$6,
 catalog_epoch=coalesce(catalog_epoch,(SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$4))
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index, source, domain.HashContent(raw), raw)
}

func docCatalogProgressDeniedPG(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, before string, failure error, code, message string) {
	test.Helper()
	docCatalogProtocolDeniedPG(test, failure, code, message)
	var postgresError *pgconn.PgError
	if !errors.As(failure, &postgresError) || !strings.Contains(postgresError.Where, "cxthub_guard_doc_catalog_job_write") {
		test.Fatal("expected the actual job-progress guard, not an unrelated rejection", failure)
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("rejected direct progress changed the job, Source, admission, objects, or private catalogs")
	}
}

func docCatalogProgressExtraPG(test *testing.T, raw []byte) []byte {
	test.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		test.Fatal(err)
	}
	value["unexpected"] = json.RawMessage(`true`)
	result, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return result
}

func TestDocDisplayCatalogProgressPGControlCannotAdvance(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	prepared := docCatalogProgressLoadPG(test, fixture, job)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	raw, err := step.Preparation().Encode()
	if err != nil || domain.HashContent(raw) == step.Prior() {
		test.Fatal("actual calculation must advance its predecessor", err)
	}
	use, sealed := step.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("actual calculation lacks its Source incarnation", use)
	}
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
		test.Fatal(err)
	}
	_, err = docCatalogProgressWritePG(fixture, transaction, job, use.Source, raw)
	docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "document catalog writes require a bound calculation")
	saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil || docCatalogUseCheckpointPG(test, saved) != domain.HashContent(raw) {
		test.Fatal("the same genuine step must remain usable through current calculation authority", err)
	}
}

func TestDocDisplayCatalogProgressPGInitializationShape(test *testing.T) {
	for _, kind := range []domain.EventKind{domain.EventReasoning, domain.EventToolResult} {
		test.Run(string(kind), func(test *testing.T) {
			fixture, job, initial := docCatalogProgressStartPG(test, kind)
			raw, err := initial.Encode()
			if err != nil {
				test.Fatal(err)
			}
			source, err := initial.Source.Hash()
			if err != nil {
				test.Fatal(err)
			}
			wrong := initial
			wrong.Phase = "tool_discovery"
			if kind == domain.EventToolResult {
				wrong.Phase = "discovery"
			}
			var wrongValue map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wrongValue); err != nil {
				test.Fatal(err)
			}
			wrongValue["phase"], err = json.Marshal(wrong.Phase)
			if err != nil {
				test.Fatal(err)
			}
			wrongRaw, err := json.Marshal(wrongValue)
			if err != nil {
				test.Fatal(err)
			}
			for _, candidate := range []struct {
				name string
				raw  []byte
			}{{"extra_field", docCatalogProgressExtraPG(test, raw)}, {"wrong_phase", wrongRaw}} {
				test.Run(candidate.name, func(test *testing.T) {
					before := docCatalogUseImagePG(test, fixture)
					transaction := docCatalogUseBeginPG(test, fixture)
					if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
						test.Fatal(err)
					}
					_, err := docCatalogProgressWritePG(fixture, transaction, job, source, candidate.raw)
					docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "55000", "document catalog writer claim is not live")
				})
			}
			transaction := docCatalogUseBeginPG(test, fixture)
			if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
				test.Fatal(err)
			}
			if tag, err := docCatalogProgressWritePG(fixture, transaction, job, source, raw); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("exact minimal initialization must remain a control operation", tag, err)
			}
			if err := transaction.Commit(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			loaded := docCatalogProgressLoadPG(test, fixture, job)
			if docCatalogUseCheckpointPG(test, loaded) != domain.HashContent(raw) {
				test.Fatal("direct exact initialization changed its canonical bytes")
			}
			step := docEventDisplayBoundaryStep(test, fixture, job, loaded)
			if docCatalogUseCheckpointPG(test, step.Preparation()) == domain.HashContent(raw) {
				test.Fatal("reinitialization denial requires an actually advanced predecessor")
			}
			if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err != nil {
				test.Fatal(err)
			}
			for _, candidate := range []struct {
				name string
				raw  []byte
			}{{"reinitialize", raw}, {"reinitialize_extra_field", docCatalogProgressExtraPG(test, raw)}} {
				test.Run(candidate.name, func(test *testing.T) {
					before := docCatalogUseImagePG(test, fixture)
					transaction := docCatalogUseBeginPG(test, fixture)
					if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
						test.Fatal(err)
					}
					_, err := docCatalogProgressWritePG(fixture, transaction, job, source, candidate.raw)
					docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "document catalog writes require a bound calculation")
				})
			}
		})
	}
}

func TestDocDisplayCatalogProgressPGRechecksGenerationAfterBinding(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	prepared := docCatalogProgressLoadPG(test, fixture, job)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	use, sealed := step.CatalogUse()
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	if !sealed || use.Validate() != nil || string(use.Source) != admission.source || use.Generation != admission.generation {
		test.Fatal("actual calculation did not capture current Source/admission agreement", use, admission)
	}
	var sourceRaw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT bytes FROM doc_display_sources_v1 WHERE source_hash=$1`, use.Source).Scan(&sourceRaw); err != nil || domain.HashContent(sourceRaw) != use.Source {
		test.Fatal("missing actual canonical Source", err)
	}
	raw, err := step.Preparation().Encode()
	if err != nil || domain.HashContent(raw) == step.Prior() {
		test.Fatal("actual calculation must change progress", err)
	}
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	if err := docCatalogProtocolCalculationPG(fixture, transaction, job, use); err != nil {
		test.Fatal(err)
	}
	for _, replacement := range [][]byte{[]byte(`{}`), bytes.Clone(sourceRaw)} {
		docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET bytes=$4
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), use.Source, replacement)
	}
	changed := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, job.Target.Event.Hash, job.Target.DisplayProfile())
	if changed.source != admission.source || changed.revision != admission.revision || changed.generation == use.Generation {
		test.Fatal("actual same-transaction Source ABA failed to replace only the incarnation", changed, admission)
	}
	_, err = docCatalogProgressWritePG(fixture, transaction, job, use.Source, raw)
	docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "55000", "document catalog source incarnation changed")
}

func TestDocDisplayCatalogProgressPGInsertConflictCannotCarryProgress(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	docCatalogProgressLoadPG(test, fixture, job)
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `SET LOCAL cxthub.doc_display_protocol='5'`); err != nil {
		test.Fatal(err)
	}
	_, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1 SELECT * FROM doc_display_jobs_v1
 WHERE `+docDisplayKey(job.Target)+` ON CONFLICT DO NOTHING`, job.RepoID, job.DocHash, job.Target.Event.Index)
	docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "document display jobs must be inserted without progress")
}

func TestDocDisplayCatalogProgressPGInactiveCannotChangeProgress(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	prepared := docCatalogProgressLoadPG(test, fixture, job)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	use, sealed := step.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("actual calculation lacks its Source incarnation", use)
	}
	raw, err := step.Preparation().Encode()
	if err != nil || domain.HashContent(raw) == step.Prior() {
		test.Fatal("actual calculation must change progress", err)
	}
	for _, state := range []string{"waiting", "retrying", "rejected"} {
		test.Run(state, func(test *testing.T) {
			before := docCatalogUseImagePG(test, fixture)
			transaction := docCatalogUseBeginPG(test, fixture)
			if err := docCatalogProtocolCalculationPG(fixture, transaction, job, use); err != nil {
				test.Fatal(err)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET state=$4,lease_until=NULL
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index, state)
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET reason='metadata only'
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index)
			_, err := transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET reason='metadata and progress',preparation_hash=$4,preparation=$5
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index, domain.HashContent(raw), raw)
			docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "inactive document display jobs cannot change progress")
		})
	}
}

func TestDocDisplayCatalogProgressPGLiveNoOpRequiresOwnLiveControl(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	docCatalogProgressLoadPG(test, fixture, job)
	foreign := docDisplayPG(test, fixture.doc.cir.Events...)
	enqueueDocEventDisplayQueue(test, foreign, 0)
	foreignJob := foreign.claim(test)
	foreignPrepared := docCatalogProgressLoadPG(test, foreign, foreignJob)
	foreignSource, err := foreignPrepared.Source.Hash()
	if err != nil || foreignJob.RepoID == job.RepoID || foreignJob.Target.Event.Hash != job.Target.Event.Hash || foreign.storage.pool.Config().ConnConfig.Database != fixture.storage.pool.Config().ConnConfig.Database {
		test.Fatal("foreign live job must share the actual Source but not the job identity", err)
	}
	for _, mode := range []string{"missing", "foreign", "expired", "current"} {
		test.Run(mode, func(test *testing.T) {
			before := docCatalogUseImagePG(test, fixture)
			transaction := docCatalogUseBeginPG(test, fixture)
			code, message := "0A000", "document catalog writes require a bound writer"
			switch mode {
			case "foreign":
				if err := docCatalogProtocolControlPG(foreign, transaction, foreignJob); err != nil {
					test.Fatal(err)
				}
				admission := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, job.Target.Event.Hash, job.Target.DisplayProfile())
				if admission.source != string(foreignSource) {
					test.Fatal("foreign proof fixture lost its shared actual Source", admission)
				}
				message = "document catalog writer binding is invalid"
			case "expired", "current":
				if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
					test.Fatal(err)
				}
				if mode == "expired" {
					docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second'
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index)
					code, message = "55000", "document catalog writer claim is not live"
				}
			}
			tag, err := transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET preparation_hash=preparation_hash,preparation=preparation
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index)
			if mode != "current" {
				docCatalogProgressDeniedPG(test, fixture, transaction, before, err, code, message)
				return
			}
			if err != nil || tag.RowsAffected() != 1 {
				test.Fatal("own live control must permit an exact no-op", tag, err)
			}
			if err := transaction.Commit(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("authorized exact no-op changed durable state")
			}
		})
	}
}

func docCatalogProgressCompletePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) (domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	for steps := 0; prepared.Phase != "completed" && steps < 64; steps++ {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("calculate and save the actual small reasoning receipt", err)
		}
	}
	if prepared.Phase != "completed" {
		test.Fatal("small reasoning receipt did not complete within its bounded fixture budget", prepared.Phase)
	}
	checkpoint := docCatalogUseCheckpointPG(test, prepared)
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, checkpoint, time.Now().UTC()); err != nil {
		test.Fatal("publish the actual completed receipt", err)
	}
	receipt, err := scanDocDisplayJob(fixture.storage.pool.QueryRow(fixture.ctx, `SELECT `+docDisplayJobColumns+`
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index))
	if err != nil || receipt.State != "completed" || receipt.Version != job.Version || !sameDocDisplayTarget(receipt.Target, job.Target) {
		test.Fatal("actual publication did not retain the completed job identity", receipt, err)
	}
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var publishedRaw, objectRaw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT published.bytes,object.bytes
 FROM doc_display_generations_v1 published JOIN doc_display_objects_v1 object
 ON object.source_hash=published.source_hash AND object.hash=published.generation_hash
 WHERE published.event_hash=$1 AND published.profile=$2 AND published.source_hash=$3 AND published.generation_hash=$4`,
		job.Target.Event.Hash, job.Target.DisplayProfile(), source, prepared.Generation).Scan(&publishedRaw, &objectRaw); err != nil || !bytes.Equal(publishedRaw, objectRaw) {
		test.Fatal("completed receipt must have its actual published CAS descriptor", err)
	}
	generation, err := domain.DecodeDocDisplayProjectionGeneration(prepared.Generation, publishedRaw)
	if err != nil || generation.Source.Plan != prepared.Plan || generation.Source.EventHash != job.Target.Event.Hash || generation.Source.EventBytes != job.Target.Event.Length || prepared.Plan == prepared.Generation {
		test.Fatal("completed receipt disagrees with its actual generation and plan", err)
	}
	return receipt, prepared
}

func docCatalogProgressCompletedPeersPG(test *testing.T, count int) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation, []domain.DocDisplayJob) {
	test.Helper()
	if count < 1 || count > 2 {
		test.Fatal("completed-peer fixture must stay bounded to one or two actual receipts", count)
	}
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	initial := docCatalogProgressLoadPG(test, fixture, job)
	peers := make([]domain.DocDisplayJob, 0, count)
	var completed domain.DocEventDisplayPreparation
	for index := 0; index < count; index++ {
		peer := docDisplayPG(test, fixture.doc.cir.Events...)
		enqueueDocEventDisplayQueue(test, peer, 0)
		peerJob := peer.claim(test)
		prepared := docCatalogProgressLoadPG(test, peer, peerJob)
		receipt, prepared := docCatalogProgressCompletePG(test, peer, peerJob, prepared)
		if peer.storage.pool.Config().ConnConfig.Database != fixture.storage.pool.Config().ConnConfig.Database || receipt.RepoID == job.RepoID || prepared.Source != initial.Source {
			test.Fatal("actual peer must share the Source and database, not the running job", receipt)
		}
		if index > 0 && docCatalogUseCheckpointPG(test, prepared) != docCatalogUseCheckpointPG(test, completed) {
			test.Fatal("actual completed reuse did not preserve the exact canonical receipt")
		}
		completed = prepared
		peers = append(peers, receipt)
	}
	source, err := completed.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var firstRepo domain.ContentHash
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT repo_id FROM doc_display_jobs_v1
 WHERE source_hash=$1 AND profile='public-event-display-v1' AND worker_protocol=5 AND state='completed'
 AND preparation_hash IS NOT NULL AND preparation IS NOT NULL ORDER BY repo_id,doc_hash,event_index LIMIT 1`, source).Scan(&firstRepo); err != nil {
		test.Fatal(err)
	}
	if len(peers) == 2 && peers[1].RepoID == firstRepo {
		peers[0], peers[1] = peers[1], peers[0]
	}
	if peers[0].RepoID != firstRepo || len(peers) == 2 && peers[0].RepoID == peers[1].RepoID {
		test.Fatal("actual ordered first completed peer was not identified", firstRepo, peers)
	}
	docCatalogProgressGuardsPG(test, fixture)
	return fixture, job, completed, peers
}

func docCatalogProgressHistoricalPeerCorruptionPG(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, peer domain.DocDisplayJob, original, replacement domain.DocEventDisplayPreparation) {
	test.Helper()
	var database string
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()`).Scan(&database); err != nil {
		test.Fatal(err)
	}
	if _, owned := docDisplayOwnedFixturesPG.Load(database); !owned || database != fixture.storage.pool.Config().ConnConfig.Database {
		test.Fatal("historical peer corruption requires this transaction's owned isolated fixture database", database)
	}
	docCatalogProgressGuardsOnPG(test, fixture, transaction)
	raw, err := replacement.Encode()
	if err != nil || original.Source != replacement.Source || docCatalogUseCheckpointPG(test, original) == domain.HashContent(raw) {
		test.Fatal("historical peer corruption must change only a valid same-Source receipt", err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET preparation_hash=$4,preparation=$5
 WHERE `+docDisplayKey(peer.Target)+` AND state='completed' AND preparation_hash=$6`,
		peer.RepoID, peer.DocHash, peer.Target.Event.Index, domain.HashContent(raw), raw, docCatalogUseCheckpointPG(test, original))
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 ENABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	docCatalogProgressGuardsOnPG(test, fixture, transaction)
}

func TestDocDisplayCatalogProgressPGCompletedPeerMustBeFirst(test *testing.T) {
	fixture, job, completed, peers := docCatalogProgressCompletedPeersPG(test, 2)
	source, err := completed.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	raw, err := completed.Encode()
	if err != nil {
		test.Fatal(err)
	}
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	corrupt := completed
	corrupt.Plan = completed.Generation
	docCatalogProgressHistoricalPeerCorruptionPG(test, fixture, transaction, peers[0], completed, corrupt)
	var firstHash domain.ContentHash
	var firstRaw, secondRaw []byte
	var firstEpoch *string
	if err := transaction.QueryRow(fixture.ctx, docEventDisplayCompletedPeerSQL, source, domain.MaxDocEventDisplayPreparationBytes+1).Scan(&firstHash, &firstRaw, &firstEpoch); err != nil || firstHash != docCatalogUseCheckpointPG(test, corrupt) || domain.HashContent(firstRaw) != firstHash {
		test.Fatal("ordered lookup must encounter the corrupt first receipt before the valid second", firstHash, err)
	}
	if err := transaction.QueryRow(fixture.ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE `+docDisplayKey(peers[1].Target)+` AND state='completed'`,
		peers[1].RepoID, peers[1].DocHash, peers[1].Target.Event.Index).Scan(&secondRaw); err != nil || !bytes.Equal(secondRaw, raw) || bytes.Equal(firstRaw, secondRaw) {
		test.Fatal("second completed peer must still retain the exact valid published receipt", err)
	}
	if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
		test.Fatal(err)
	}
	docCatalogProgressGuardsOnPG(test, fixture, transaction)
	_, err = docCatalogProgressWritePG(fixture, transaction, job, source, raw)
	docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "document catalog writes require a bound calculation")
	current := docCatalogUseBeginPG(test, fixture)
	if err := docCatalogProtocolControlPG(fixture, current, job); err != nil {
		test.Fatal(err)
	}
	if tag, err := docCatalogProgressWritePG(fixture, current, job, source, raw); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("exact current first published peer must remain control-copyable", tag, err)
	}
	if err := current.Rollback(fixture.ctx); err != nil || docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("positive peer-copy probe did not roll back to the original image", err)
	}
}

func TestDocDisplayCatalogProgressPGCompletedPeerRequiresPublicationAgreement(test *testing.T) {
	fixture, job, completed, peers := docCatalogProgressCompletedPeersPG(test, 1)
	source, err := completed.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var planRaw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, source, completed.Plan).Scan(&planRaw); err != nil || domain.HashContent(planRaw) != completed.Plan || !json.Valid(planRaw) || len(planRaw) > domain.MaxDocDisplaySourceBytes {
		test.Fatal("publication disagreement must use the actual bounded same-Source plan object", err)
	}
	for _, disagreement := range []string{"missing_publication", "generation_hash", "descriptor_bytes", "peer_plan"} {
		test.Run(disagreement, func(test *testing.T) {
			before := docCatalogUseImagePG(test, fixture)
			transaction := docCatalogUseBeginPG(test, fixture)
			candidate := completed
			if disagreement == "peer_plan" {
				candidate.Plan = completed.Generation
				docCatalogProgressHistoricalPeerCorruptionPG(test, fixture, transaction, peers[0], completed, candidate)
			}
			if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
				test.Fatal(err)
			}
			switch disagreement {
			case "missing_publication":
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), source)
			case "generation_hash":
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_generations_v1 SET generation_hash=$4
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), source, completed.Plan)
			case "descriptor_bytes":
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_generations_v1 SET bytes=$4
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), source, planRaw)
			}
			raw, err := candidate.Encode()
			if err != nil {
				test.Fatal(err)
			}
			var firstHash domain.ContentHash
			var firstRaw []byte
			var firstEpoch *string
			if err := transaction.QueryRow(fixture.ctx, docEventDisplayCompletedPeerSQL, source, domain.MaxDocEventDisplayPreparationBytes+1).Scan(&firstHash, &firstRaw, &firstEpoch); err != nil || firstHash != domain.HashContent(raw) || !bytes.Equal(firstRaw, raw) {
				test.Fatal("publication rejection must start from an exact ordered-first peer match", firstHash, err)
			}
			docCatalogProgressGuardsOnPG(test, fixture, transaction)
			_, err = docCatalogProgressWritePG(fixture, transaction, job, source, raw)
			docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "document catalog writes require a bound calculation")
		})
	}
}

func TestDocDisplayCatalogProgressPGCompletedCannotChangeProgress(test *testing.T) {
	fixture, job, _ := docCatalogProgressStartPG(test, domain.EventReasoning)
	initial := docCatalogProgressLoadPG(test, fixture, job)
	receipt, completed := docCatalogProgressCompletePG(test, fixture, job, initial)
	raw, err := initial.Encode()
	if err != nil || domain.HashContent(raw) == docCatalogUseCheckpointPG(test, completed) {
		test.Fatal("inactive completed mutation must actually change the saved receipt", err)
	}
	for _, mutation := range []string{"progress_only", "metadata_and_progress"} {
		test.Run(mutation, func(test *testing.T) {
			before := docCatalogUseImagePG(test, fixture)
			transaction := docCatalogUseBeginPG(test, fixture)
			docCatalogProgressGuardsOnPG(test, fixture, transaction)
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET state=state,preparation=preparation
 WHERE `+docDisplayKey(receipt.Target), receipt.RepoID, receipt.DocHash, receipt.Target.Event.Index)
			statement := `UPDATE doc_display_jobs_v1 SET preparation_hash=$4,preparation=$5`
			if mutation == "metadata_and_progress" {
				statement += `,reason='completed metadata and progress'`
			}
			_, err := transaction.Exec(fixture.ctx, statement+` WHERE `+docDisplayKey(receipt.Target),
				receipt.RepoID, receipt.DocHash, receipt.Target.Event.Index, domain.HashContent(raw), raw)
			docCatalogProgressDeniedPG(test, fixture, transaction, before, err, "0A000", "inactive document display jobs cannot change progress")
		})
	}
}
