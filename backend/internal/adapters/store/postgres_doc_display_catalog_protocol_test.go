//go:build postgres

package store

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docCatalogProtocolReplayPG struct {
	operation string
	statement string
	rows      int64
}

func docCatalogProtocolReplaysPG(table string) []docCatalogProtocolReplayPG {
	quoted := pgx.Identifier{table}.Sanitize()
	columns := ""
	switch table {
	case "doc_tool_tree_sources_v1":
		columns = "scope,source_hash,bytes,root_from,root_to"
	case "doc_tool_key_sources_v1":
		columns = "scope,tree_scope,bytes"
	case "doc_tool_order_sources_v1":
		columns = "scope,source_hash,tree_scope,keys_scope,bytes"
	}
	selection, target := "*", ""
	if columns != "" {
		selection, target = columns, "("+columns+")"
	}
	return []docCatalogProtocolReplayPG{
		{operation: "insert", statement: `INSERT INTO ` + quoted + target + ` SELECT ` + selection + ` FROM ` + quoted + ` LIMIT 1 ON CONFLICT DO NOTHING`},
		{operation: "update", statement: `UPDATE ` + quoted + ` SET scope=scope WHERE ctid=(SELECT ctid FROM ` + quoted + ` LIMIT 1)`, rows: 1},
	}
}

func docCatalogProtocolFixturePG(test *testing.T) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	fixture, job, prepared := docCatalogUseFixturePG(test)
	prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
	if prepared.Phase != "tool_orders_done" {
		test.Fatal("private replay fixture did not complete its actual order plans", prepared.Phase)
	}
	for _, table := range docCatalogWriterTablesPG {
		var count int
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count == 0 {
			test.Fatal("private replay fixture must populate every guarded table", table, count, err)
		}
	}
	return fixture, job, prepared
}

func docCatalogProtocolControlPG(fixture docDisplayPGFixture, transaction pgx.Tx, job domain.DocDisplayJob) error {
	_, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version)
	return err
}

func docCatalogProtocolCalculationPG(fixture docDisplayPGFixture, transaction pgx.Tx, job domain.DocDisplayJob, use domain.DocDisplayCatalogUse) error {
	var prior *domain.ContentHash
	if err := transaction.QueryRow(fixture.ctx, `SELECT preparation_hash FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&prior); err != nil {
		return err
	}
	_, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid,$8::uuid,$9)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, use.Source, use.Generation, use.Epoch, prior)
	return err
}

func docCatalogJobEpochPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) string {
	test.Helper()
	var epoch string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT catalog_epoch::text FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&epoch); err != nil || domain.ValidateDocDisplayCatalogEpoch(epoch) != nil {
		test.Fatal("job must retain its original catalog epoch", epoch, err)
	}
	return epoch
}

func docCatalogProtocolDeniedPG(test *testing.T, failure error, code, message string) {
	test.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(failure, &postgresError) || postgresError.Code != code || postgresError.Message != message {
		test.Fatal("expected the exact catalog authority rejection", failure, code, message)
	}
}

func docCatalogProtocolReplayCurrentPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, use domain.DocDisplayCatalogUse) {
	test.Helper()
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if err := docCatalogProtocolCalculationPG(fixture, transaction, job, use); err != nil {
		test.Fatal("current-generation SQL calculation binder must exist and succeed", err)
	}
	for _, table := range docCatalogWriterTablesPG {
		for _, replay := range docCatalogProtocolReplaysPG(table) {
			tag, err := transaction.Exec(fixture.ctx, replay.statement)
			if err != nil || tag.RowsAffected() != replay.rows {
				test.Fatal("current-generation calculation rights rejected exact private replay", table, replay.operation, tag, err)
			}
		}
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal("commit authorized exact private replays", err)
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("authorized exact private replays changed durable state")
	}
}

func TestDocDisplayCatalogProtocolPGLegacyBinderCannotRefreshIncarnation(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	calculated := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	use, sealed := calculated.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("actual read calculation did not capture its source incarnation", use, sealed)
	}
	sourceRaw, err := prepared.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	current := docCatalogEpochABAPG(test, fixture, job, sourceRaw)
	if current.source != string(use.Source) || current.generation == use.Generation || domain.HashContent(sourceRaw) != use.Source {
		test.Fatal("same-source ABA did not invalidate the captured calculation", use, current)
	}
	before := docCatalogUseImagePG(test, fixture)
	legacy := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(legacy)
	_, err = legacy.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_writer($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version)
	docCatalogWriterCodePG(test, err, "0A000")
	rollbackPG(legacy)
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("legacy binder rejection changed the job, source, objects, or private catalogs")
	}
}

func TestDocDisplayCatalogProtocolPGControlCannotReplayPrivateCatalogs(test *testing.T) {
	fixture, job, _ := docCatalogProtocolFixturePG(test)
	before := docCatalogUseImagePG(test, fixture)
	for _, table := range docCatalogWriterTablesPG {
		test.Run(table, func(test *testing.T) {
			for _, replay := range docCatalogProtocolReplaysPG(table) {
				transaction := docCatalogUseBeginPG(test, fixture)
				if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
					test.Fatal("live SQL control binder must exist and succeed before checking private denial", err)
				}
				_, err := transaction.Exec(fixture.ctx, replay.statement)
				docCatalogProtocolDeniedPG(test, err, "0A000", "document catalog writes require a bound calculation")
				rollbackPG(transaction)
				if docCatalogUseImagePG(test, fixture) != before {
					test.Fatal("control-only private replay changed durable state", replay.operation)
				}
			}
		})
	}
}

func TestDocDisplayCatalogProtocolPGCalculationRequiresCurrentGeneration(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	use := domain.DocDisplayCatalogUse{Source: domain.ContentHash(admission.source), Generation: admission.generation, Epoch: docCatalogJobEpochPG(test, fixture, job)}
	if err := use.Validate(); err != nil {
		test.Fatal(err)
	}
	docCatalogProtocolReplayCurrentPG(test, fixture, job, use)
	sourceRaw, err := prepared.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	current := docCatalogEpochABAPG(test, fixture, job, sourceRaw)
	before := docCatalogUseImagePG(test, fixture)
	stale := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(stale)
	docCatalogProtocolDeniedPG(test, docCatalogProtocolCalculationPG(fixture, stale, job, use), "55000", "document catalog source incarnation changed")
	rollbackPG(stale)
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("stale-generation SQL calculation binder changed durable state")
	}
	use.Generation = current.generation
	docCatalogProtocolReplayCurrentPG(test, fixture, job, use)
}

func TestDocDisplayCatalogProtocolPGPrivateReplayRechecksGenerationAfterBinding(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	sourceRaw, err := prepared.Source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	use := domain.DocDisplayCatalogUse{Source: domain.HashContent(sourceRaw), Generation: admission.generation, Epoch: docCatalogJobEpochPG(test, fixture, job)}
	before := docCatalogUseImagePG(test, fixture)
	for _, table := range docCatalogWriterTablesPG {
		test.Run(table, func(test *testing.T) {
			for _, replay := range docCatalogProtocolReplaysPG(table) {
				transaction := docCatalogUseBeginPG(test, fixture)
				if err := docCatalogProtocolCalculationPG(fixture, transaction, job, use); err != nil {
					test.Fatal("bind genuine current-generation calculation rights before in-transaction ABA", err)
				}
				for _, replacement := range [][]byte{[]byte(`{}`), bytes.Clone(sourceRaw)} {
					docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET bytes=$4
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3`, job.Target.Event.Hash, job.Target.DisplayProfile(), use.Source, replacement)
				}
				changed := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, job.Target.Event.Hash, job.Target.DisplayProfile())
				if changed.source != admission.source || changed.revision != admission.revision || changed.generation == use.Generation {
					test.Fatal("actual admission trigger did not replace the bound generation inside the same transaction", changed, use)
				}
				_, err := transaction.Exec(fixture.ctx, replay.statement)
				docCatalogProtocolDeniedPG(test, err, "55000", "document catalog source incarnation changed")
				rollbackPG(transaction)
				if docCatalogUseImagePG(test, fixture) != before {
					test.Fatal("private replay trusted stale calculation flags or leaked its rolled-back ABA", replay.operation)
				}
			}
		})
	}
}

func TestDocDisplayCatalogProtocolPGClaimsAndControlRenewKeepFormatterIdentity(test *testing.T) {
	for _, profile := range []string{domain.DocDisplayProjectionProfile, domain.DocDisplayStringProfile} {
		test.Run(profile, func(test *testing.T) {
			rootProjectionIsolatedPG(test)
			fixture := docDisplayPG(test, domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: test.Name(), Output: "small protocol fixture"})
			fixture = docCatalogUseBoundPG(test, fixture)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			queued, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now())
			if err != nil {
				test.Fatal(err)
			}
			displayProtocol, workerProtocol := "5", 5
			if profile == domain.DocDisplayStringProfile {
				displayProtocol, workerProtocol = "2", 2
			}
			claimSQL := `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,attempts=attempts+1,
 lease_until=clock_timestamp()+interval '1 minute' WHERE ` + docDisplayKey(target)
			before := docCatalogUseImagePG(test, fixture)
			for _, previous := range []string{"1", "2"} {
				legacy := docCatalogUseBeginPG(test, fixture)
				if _, err := legacy.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true),set_config('cxthub.doc_catalog_protocol',$2,true)`, displayProtocol, previous); err != nil {
					test.Fatal(err)
				}
				_, err = legacy.Exec(fixture.ctx, claimSQL, queued.RepoID, queued.DocHash, queued.Target.Event.Index)
				docCatalogWriterCodePG(test, err, "0A000")
				rollbackPG(legacy)
				if docCatalogUseImagePG(test, fixture) != before {
					test.Fatal("old claim changed a current formatter job", previous)
				}
			}
			claim := docCatalogUseBeginPG(test, fixture)
			if _, err := claim.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true),set_config('cxthub.doc_catalog_protocol','3',true)`, displayProtocol); err != nil {
				test.Fatal(err)
			}
			job, err := scanDocDisplayJob(claim.QueryRow(fixture.ctx, claimSQL+` RETURNING `+docDisplayJobColumns,
				queued.RepoID, queued.DocHash, queued.Target.Event.Index))
			if err != nil || job.State != "running" || job.Version != queued.Version+1 || job.Attempts != queued.Attempts+1 || job.Target.DisplayProfile() != profile {
				test.Fatal("protocol-3 SQL claim did not preserve the formatter job identity", job, err)
			}
			if err := claim.Commit(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if profile == domain.DocDisplayProjectionProfile {
				if _, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job); err != nil {
					test.Fatal(err)
				}
			} else {
				fixture.initialize(test, job)
			}
			admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
			control := docCatalogUseBeginPG(test, fixture)
			if err := docCatalogProtocolControlPG(fixture, control, job); err != nil {
				test.Fatal("protocol-2 SQL control binder rejected a live source-bound job", err)
			}
			probe := docCatalogUseBeginPG(test, fixture)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, target, false)
			_, err = probe.Exec(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`, admission.source)
			docCatalogWriterCodePG(test, err, "55P03")
			rollbackPG(probe)
			beforeRenew := docCatalogUseImagePG(test, fixture)
			tag, err := control.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET lease_until=lease_until WHERE `+docDisplayKey(target),
				job.RepoID, job.DocHash, job.Target.Event.Index)
			if err != nil || tag.RowsAffected() != 1 {
				test.Fatal("control-only SQL authority could not perform a valid renewal", tag, err)
			}
			if err := control.Commit(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			var storedProtocol int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT worker_protocol FROM doc_display_jobs_v1 WHERE `+docDisplayKey(target),
				job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&storedProtocol); err != nil || storedProtocol != workerProtocol || docCatalogUseImagePG(test, fixture) != beforeRenew {
				test.Fatal("catalog protocol upgrade changed formatter protocol, incarnation, or job contents during control renewal", storedProtocol, err)
			}
		})
	}
}
