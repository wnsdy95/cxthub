//go:build postgres

package store

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// runtimeRightsAccount makes an account of the server that is not the owner
// of any table, and removes it with what it was given when the test ends.
func runtimeRightsAccount(t *testing.T, fixture *processAdmissionPGFixture, prefix string) string {
	t.Helper()
	role := domain.NewID(prefix)
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := fixture.observer.Exec(fixture.ctx, "CREATE ROLE "+quoted+" NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.observer.Exec(fixture.ctx, "DROP OWNED BY "+quoted)
		_, _ = fixture.observer.Exec(fixture.ctx, "DROP ROLE "+quoted)
	})
	if _, err := fixture.observer.Exec(fixture.ctx, "GRANT USAGE ON SCHEMA public TO "+quoted); err != nil {
		t.Fatal(err)
	}
	return role
}

// runtimeRight answers whether an account may do what on a table.
func runtimeRight(t *testing.T, fixture *processAdmissionPGFixture, role, table, right string) bool {
	t.Helper()
	var may bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT has_table_privilege($1, $2, $3)`, role, "public."+table, right).Scan(&may); err != nil {
		t.Fatal(err)
	}
	return may
}

// On a database the migrations made, an account of the server may not use the
// tables of 0161 to 0170, whatever it may do on repos. The rights script gives
// each account, on each of those tables, the rights it has on repos: all four
// to the account that writes repos, reading to the one that reads it, and
// nothing to an account without rights on repos. It grants nothing else, and
// running it again changes nothing.
func TestPGRuntimeRightsGiveTheNewTablesTheRightsOfRepos(t *testing.T) {
	fixture, storage := migrationVerificationStorePG(t)
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		t.Fatal(err)
	}
	writer := runtimeRightsAccount(t, fixture, "cxt_rights_writer_")
	reader := runtimeRightsAccount(t, fixture, "cxt_rights_reader_")
	other := runtimeRightsAccount(t, fixture, "cxt_rights_other_")
	for _, statement := range []string{
		"GRANT SELECT, INSERT, UPDATE, DELETE ON public.repos TO " + pgx.Identifier{writer}.Sanitize(),
		"GRANT SELECT ON public.repos TO " + pgx.Identifier{reader}.Sanitize(),
	} {
		if _, err := fixture.observer.Exec(fixture.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	tables := runtimeRightsTables(t, runtimeRights0161)
	for _, table := range tables {
		if runtimeRight(t, fixture, writer, table, "SELECT") {
			t.Fatalf("before the script, the writer may read %s", table)
		}
	}
	script, err := os.ReadFile(runtimeRights0161)
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := fixture.observer.Exec(fixture.ctx, string(script)); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		for _, table := range tables {
			for _, right := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
				if !runtimeRight(t, fixture, writer, table, right) {
					t.Fatalf("run %d: the writer may not %s on %s", run, right, table)
				}
				if got := runtimeRight(t, fixture, reader, table, right); got != (right == "SELECT") {
					t.Fatalf("run %d: the reader may %s on %s: %v", run, right, table, got)
				}
				if runtimeRight(t, fixture, other, table, right) {
					t.Fatalf("run %d: an account without rights on repos may %s on %s", run, right, table)
				}
			}
			for _, right := range []string{"TRUNCATE", "REFERENCES", "TRIGGER"} {
				if runtimeRight(t, fixture, writer, table, right) {
					t.Fatalf("run %d: the writer was given %s on %s", run, right, table)
				}
			}
		}
		if runtimeRight(t, fixture, writer, "schema_migrations", "INSERT") {
			t.Fatalf("run %d: the writer may write the migration history", run)
		}
	}
}

// The script stops, and grants nothing, where the migrations were not applied
// or no account but the owner may use repos.
func TestPGRuntimeRightsRefuseWhatTheyCannotDo(t *testing.T) {
	fixture, storage := migrationVerificationStorePG(t)
	script, err := os.ReadFile(runtimeRights0161)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.observer.Exec(fixture.ctx, string(script)); err == nil || !strings.Contains(err.Error(), "apply migrations 0161 to 0170 first") {
		t.Fatalf("before the migrations: %v", err)
	}
	if _, err := fixture.observer.Exec(fixture.ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.observer.Exec(fixture.ctx, string(script)); err == nil || !strings.Contains(err.Error(), "nothing was granted") {
		t.Fatalf("without an account on repos: %v", err)
	}
}
