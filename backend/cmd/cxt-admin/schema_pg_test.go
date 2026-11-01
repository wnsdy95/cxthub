//go:build postgres

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMigrationsPGActualCommandOwnsFreshDatabase(test *testing.T) {
	baseDSN := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if baseDSN == "" {
		test.Skip("CXT_TEST_DSN unset; requires a disposable local PostgreSQL fixture")
	}
	localConfig := func(dsn string) *pgxpool.Config {
		test.Helper()
		configuration, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			test.Fatal("invalid local PostgreSQL fixture configuration")
		}
		local := func(host string) bool {
			address := net.ParseIP(host)
			return host == "localhost" || filepath.IsAbs(host) || address != nil && address.IsLoopback()
		}
		if !local(configuration.ConnConfig.Host) {
			test.Fatal("migration fixture refuses nonlocal PostgreSQL")
		}
		for _, fallback := range configuration.ConnConfig.Fallbacks {
			if fallback == nil || !local(fallback.Host) {
				test.Fatal("migration fixture refuses nonlocal PostgreSQL fallbacks")
			}
		}
		configuration.MaxConns, configuration.MinConns, configuration.MinIdleConns = 2, 0, 0
		return configuration
	}
	configuration := localConfig(baseDSN)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.NewWithConfig(ctx, configuration.Copy())
	if err != nil {
		test.Fatal("cannot open local migration fixture administrator")
	}
	database := domain.NewID("cxt_admin_migrate_")
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	var observer *pgxpool.Pool
	test.Cleanup(func() {
		if observer != nil {
			observer.Close()
		}
		defer admin.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+quotedDatabase+" WITH (FORCE)"); err != nil {
			test.Error("cannot drop owned migration fixture database")
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quotedDatabase+" TEMPLATE template0"); err != nil {
		test.Fatal("cannot create owned migration fixture database")
	}
	const secret = "private-migration-command-pg-detail"
	ownedDSN := baseDSN + " dbname=" + database + " application_name=" + secret + " search_path=public"
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		parsed, err := url.Parse(baseDSN)
		if err != nil {
			test.Fatal("invalid migration fixture PostgreSQL URL")
		}
		parsed.Path, parsed.RawPath = "/"+database, ""
		query := parsed.Query()
		query.Set("dbname", database)
		query.Set("application_name", secret)
		query.Set("search_path", "public")
		parsed.RawQuery = query.Encode()
		ownedDSN = parsed.String()
	}
	configuration = localConfig(ownedDSN)
	if configuration.ConnConfig.Database != database {
		test.Fatal("migration command DSN escaped its owned database")
	}
	observer, err = pgxpool.NewWithConfig(ctx, configuration.Copy())
	if err != nil {
		test.Fatal("cannot open owned migration fixture observer")
	}
	test.Setenv("CXT_POSTGRES_DSN", ownedDSN)
	test.Setenv("CXT_MIGRATIONS_DIR", "unused-migration-fixture-directory")
	command := func(apply, mismatch bool) migrationResult {
		test.Helper()
		args := []string{"--dir", "../../../schemas/db/migrations"}
		mode := "check"
		if apply {
			args, mode = append(args, "--apply"), "apply"
		}
		var stdout, stderr bytes.Buffer
		err := runMigrations(ctx, args, &stdout, &stderr)
		output := stdout.String() + stderr.String()
		if err != nil {
			output += err.Error()
		}
		if strings.Contains(output, secret) || strings.Contains(output, ownedDSN) || stderr.Len() != 0 {
			test.Fatal("actual migration command disclosed private details or emitted unexpected diagnostics")
		}
		if mismatch {
			if !errors.Is(err, store.ErrSchemaMismatch) || stdout.Len() != 0 {
				test.Fatal("actual migration command failed to refuse schema mismatch without success JSON")
			}
			return migrationResult{}
		}
		var result migrationResult
		if err != nil || json.Unmarshal(stdout.Bytes(), &result) != nil || !result.Verified || result.Mode != mode || result.Applied < 0 {
			test.Fatal("actual migration command did not report verified success")
		}
		return result
	}
	command(false, true)
	var historyAbsent bool
	if err := observer.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relname='schema_migrations')`).Scan(&historyAbsent); err != nil || !historyAbsent {
		test.Fatal("fresh default check created migration history")
	}
	files, err := filepath.Glob("../../../schemas/db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		test.Fatal("current complete migration bundle is unavailable")
	}
	if result := command(true, false); result.Applied != len(files) {
		test.Fatal("actual operator apply did not install the complete current bundle")
	}
	history := func() string {
		test.Helper()
		var snapshot string
		if err := observer.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(history) ORDER BY version)::text,'[]') FROM public.schema_migrations history`).Scan(&snapshot); err != nil {
			test.Fatal("cannot inspect owned migration history")
		}
		return snapshot
	}
	baseline := history()
	var records []json.RawMessage
	if json.Unmarshal([]byte(baseline), &records) != nil || len(records) != len(files) {
		test.Fatal("actual apply left incomplete migration history")
	}
	if result := command(false, false); result.Applied != 0 || history() != baseline {
		test.Fatal("second actual check changed migration history")
	}
	changed, err := observer.Exec(ctx, `UPDATE public.schema_migrations SET checksum=$1 WHERE version='0001_init.sql'`, secret)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("cannot corrupt the owned migration checksum fixture")
	}
	corrupted := history()
	for _, apply := range []bool{false, true} {
		command(apply, true)
		if history() != corrupted {
			test.Fatal("actual check or operator apply automatically repaired corrupt history")
		}
	}
}
