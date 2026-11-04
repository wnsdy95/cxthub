//go:build postgres

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func execDocToolCatalogPG(fixture docDisplayPGFixture, job domain.DocDisplayJob, statement string, arguments ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		var err error
		tag, err = transaction.Exec(bound, statement, arguments...)
		return err
	})
	return tag, err
}

func bindDocToolCatalogCalculationPG(ctx context.Context, transaction pgx.Tx, job domain.DocDisplayJob) error {
	var source domain.ContentHash
	var generation, epoch string
	var prior domain.ContentHash
	if err := transaction.QueryRow(ctx, `SELECT admission.source_hash,admission.generation::text,job.catalog_epoch::text,job.preparation_hash FROM doc_display_jobs_v1 job
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=job.event_hash AND admission.profile=job.profile
 AND admission.source_hash=job.source_hash WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.event_index=$3 AND job.profile=$4`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).Scan(&source, &generation, &epoch, &prior); err != nil {
		return err
	}
	_, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid,$8::uuid,$9)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, source, generation, epoch, prior)
	return err
}

func requireDocToolCatalogPGIsolation(test *testing.T, fixture docDisplayPGFixture) {
	test.Helper()
	database := fixture.storage.pool.Config().ConnConfig.Database
	if _, owned := docDisplayOwnedFixturesPG.Load(database); !owned {
		test.Fatal("catalog writer guard changes require an explicitly isolated test database")
	}
}

func setDocToolCatalogPGWriterGuards(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, tables []string, enabled bool) {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	operation := "DISABLE"
	if enabled {
		if _, err := transaction.Exec(fixture.ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			test.Fatal(err)
		}
		operation = "ENABLE"
	}
	for _, table := range tables {
		switch table {
		case "doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_key_sources_v1",
			"doc_tool_key_observations_v1", "doc_tool_key_members_v1", "doc_tool_order_sources_v1", "doc_tool_order_plans_v1":
		default:
			test.Fatal("unexpected private catalog table", table)
		}
		if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` `+operation+
			` TRIGGER `+pgx.Identifier{table + "_a_catalog_writer"}.Sanitize()); err != nil {
			test.Fatal(err)
		}
	}
}

func execDocToolCatalogPGWithoutWriter(test *testing.T, fixture docDisplayPGFixture, tables []string, statement string, arguments ...any) (pgconn.CommandTag, error) {
	test.Helper()
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, false)
	tag, err := transaction.Exec(fixture.ctx, statement, arguments...)
	if err != nil {
		return tag, err
	}
	setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, true)
	return tag, transaction.Commit(fixture.ctx)
}

func setDocToolCatalogPGDeleteGuards(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, tables []string, enabled bool) {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	operation := "DISABLE"
	if enabled {
		operation = "ENABLE"
		if _, err := transaction.Exec(fixture.ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			test.Fatal(err)
		}
	}
	for _, table := range tables {
		known := false
		for _, allowed := range docCatalogWriterTablesPG {
			known = known || table == allowed
		}
		if !known {
			test.Fatal("unexpected private delete guard table", table)
		}
		if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` `+operation+` TRIGGER `+pgx.Identifier{table + "_a_catalog_delete"}.Sanitize()); err != nil {
			test.Fatal(err)
		}
		if enabled {
			var restored bool
			if err := transaction.QueryRow(fixture.ctx, `SELECT count(*)=1 AND bool_and(tgenabled='O') FROM pg_trigger
 WHERE tgrelid=$1::regclass AND tgname=$2`, table, table+"_a_catalog_delete").Scan(&restored); err != nil || !restored {
				test.Fatal("owned corruption did not restore the actual delete guard", table, restored, err)
			}
		}
	}
}

func execDocToolCatalogPGWithCorruptDeletes(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, tables []string, statement string, arguments ...any) (pgconn.CommandTag, error) {
	test.Helper()
	var tag pgconn.CommandTag
	err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		setDocToolCatalogPGDeleteGuards(test, fixture, transaction, tables, false)
		var complete []string
		for _, table := range tables {
			var found bool
			if err := transaction.QueryRow(bound, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname=$2)`, table, table+"_complete_guard").Scan(&found); err != nil {
				return err
			}
			if !found {
				continue
			}
			complete = append(complete, table)
			if _, err := transaction.Exec(bound, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` DISABLE TRIGGER `+pgx.Identifier{table + "_complete_guard"}.Sanitize()); err != nil {
				return err
			}
		}
		var err error
		tag, err = transaction.Exec(bound, statement, arguments...)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(bound, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		for _, table := range complete {
			if _, err := transaction.Exec(bound, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` ENABLE TRIGGER `+pgx.Identifier{table + "_complete_guard"}.Sanitize()); err != nil {
				return err
			}
		}
		setDocToolCatalogPGDeleteGuards(test, fixture, transaction, tables, true)
		return nil
	})
	return tag, err
}
