//go:build postgres

package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepoAddressMover = (*PostgresStore)(nil)

// repoAddressHistory are tables that record what happened. An identifier
// they name stays as it was then; the move reports it.
var repoAddressHistory = map[string]bool{
	"schema_migrations":         true,
	"ownership_migrations":      true,
	"storage_usage_ledger":      true,
	"account_audit":             true,
	"organization_audit_events": true,
	"enterprise_audit_events":   true,
}

// repoAddressContent says whether a table holds conversation content: the
// stored documents and what is made from them, keyed by a document's hash and
// not by a repository. A conversation can quote an identifier, and what was
// said stays as it was. These tables are neither changed nor searched.
func repoAddressContent(table string, columns []repoAddressColumn) bool {
	if table == "blobs" {
		return true
	}
	if !strings.HasPrefix(table, "doc_") {
		return false
	}
	for _, column := range columns {
		if column.name == "repo_id" {
			return false
		}
	}
	return true
}

// repoAddressColumn is a column the identifier can be written in.
type repoAddressColumn struct {
	name, kind string
}

// text is the column read as text, the way it is searched.
func (c repoAddressColumn) text() string {
	name := pgx.Identifier{c.name}.Sanitize()
	switch c.kind {
	case "text", "character varying":
		return name
	case "bytea":
		return "encode(" + name + ", 'escape')"
	}
	return name + "::text"
}

// rewritable says whether the identifier can be replaced inside the column:
// text and JSON are. In anything else it stops the move.
func (c repoAddressColumn) rewritable() bool {
	switch c.kind {
	case "text", "character varying", "json", "jsonb":
		return true
	}
	return false
}

// replaced is the column with the identifier replaced, as its own type.
func (c repoAddressColumn) replaced() string {
	value := "replace(" + c.text() + ", $1, $2)"
	switch c.kind {
	case "character varying":
		return value + "::varchar"
	case "json":
		return value + "::json"
	case "jsonb":
		return value + "::jsonb"
	}
	return value
}

// repoAddressSearched are the types the identifier can be written in. A
// number, a time or a flag cannot hold it.
var repoAddressSearched = map[string]bool{
	"text": true, "character varying": true, "json": true, "jsonb": true, "bytea": true, "ARRAY": true, "USER-DEFINED": true,
}

// repoAddressTables lists the tables of the schema with their columns that
// can hold an identifier. The repository table itself is moved on its own.
func repoAddressTables(ctx context.Context, tx pgx.Tx) (map[string][]repoAddressColumn, error) {
	rows, err := tx.Query(ctx, `SELECT c.table_name, c.column_name, c.data_type
FROM information_schema.columns c JOIN information_schema.tables t USING (table_schema, table_name)
WHERE c.table_schema = 'public' AND t.table_type = 'BASE TABLE'
ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string][]repoAddressColumn{}
	for rows.Next() {
		var table, name, kind string
		if err := rows.Scan(&table, &name, &kind); err != nil {
			return nil, err
		}
		if table != "repos" && repoAddressSearched[kind] {
			tables[table] = append(tables[table], repoAddressColumn{name: name, kind: kind})
		}
	}
	return tables, rows.Err()
}

// repoAddressFound is a column that holds the identifier, and in how many rows.
type repoAddressFound struct {
	table  string
	column repoAddressColumn
	rows   int64
}

// repoAddressSearch counts, in every column of the given tables, the rows
// that hold the identifier.
func repoAddressSearch(ctx context.Context, tx pgx.Tx, tables map[string][]repoAddressColumn, names []string, id domain.ContentHash) ([]repoAddressFound, error) {
	var found []repoAddressFound
	for _, table := range names {
		for _, column := range tables[table] {
			var rows int64
			query := fmt.Sprintf(`SELECT count(*) FROM public.%s WHERE strpos(%s, $1) > 0`, pgx.Identifier{table}.Sanitize(), column.text())
			if err := tx.QueryRow(ctx, query, string(id)).Scan(&rows); err != nil {
				return nil, fmt.Errorf("searching %s.%s: %w", table, column.name, err)
			}
			if rows > 0 {
				found = append(found, repoAddressFound{table: table, column: column, rows: rows})
			}
		}
	}
	return found, nil
}

// MoveRepoAddress gives the stored repository request.From the identifier
// request.To and the address request.ToURL, in one transaction:
//
//   - The stored repository must belong to request.RepositoryID, and nothing
//     may be stored under the new identifier yet.
//   - Document jobs that are still running stop it: a job's own identifier is
//     computed from the repository's. Finished jobs are removed.
//   - Every column of every table that names the old identifier is given the
//     new one, in one statement, so that every reference agrees at its end.
//     The tables' own triggers are switched off meanwhile: they would record
//     the change as work on the old repository.
//   - Conversation content, content addressed by its own hash, and records
//     of what happened are left as they are. The old identifier left in any
//     other column, or in a column that cannot be rewritten, stops the move.
//   - What is stored is counted by its owner's space and its content's hash,
//     neither of which a move changes, so nothing is counted again.
//
// Without request.Apply the transaction is rolled back at its end, and the
// answer says what the move would change.
func (s *PostgresStore) MoveRepoAddress(ctx context.Context, request domain.RepoAddressMoveRequest) (domain.RepoAddressMove, error) {
	out := domain.RepoAddressMove{From: request.From, To: request.To, ToURL: request.ToURL, RepositoryID: request.RepositoryID, Changed: []domain.RepoAddressColumn{}, Kept: []domain.RepoAddressColumn{}}
	if err := domain.ValidateContentHash(request.From); err != nil {
		return out, err
	}
	if err := domain.ValidateContentHash(request.To); err != nil {
		return out, err
	}
	if err := domain.ValidateRepositoryID(request.RepositoryID); err != nil {
		return out, err
	}
	if request.From == request.To || domain.HashContent([]byte(domain.NormalizeGitRemoteURL(request.ToURL))) != request.To {
		return out, fmt.Errorf("%w: the new identifier is not the hash of the new address", domain.ErrValidation)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, err
	}
	defer rollbackPG(tx)
	// Waiting for the running server's locks has an end: stop it, or try again.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '15s'`); err != nil {
		return out, err
	}
	// Every repository and identity write takes this lock shared.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-identity-access',0))`); err != nil {
		return out, err
	}
	for _, repo := range []domain.ContentHash{request.From, request.To} {
		if err := lockRepoGraph(ctx, tx, repo); err != nil {
			return out, err
		}
	}
	var remote, repository string
	if err := tx.QueryRow(ctx, `SELECT remote_url, COALESCE(repository_id,'') FROM repos WHERE id=$1 FOR UPDATE`, string(request.From)).Scan(&remote, &repository); err != nil {
		return out, mapNoRows(err)
	}
	if domain.HashContent([]byte(domain.NormalizeGitRemoteURL(remote))) != request.From {
		return out, fmt.Errorf("%w: the stored repository's identifier is not the hash of its address", domain.ErrIntegrity)
	}
	if repository != request.RepositoryID {
		return out, fmt.Errorf("%w: the stored repository belongs to another workspace", domain.ErrConflict)
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repos WHERE id=$1)`, string(request.To)).Scan(&taken); err != nil {
		return out, err
	}
	if taken {
		return out, fmt.Errorf("%w: a repository is already stored under the new address", domain.ErrConflict)
	}
	var running int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, string(request.From)).Scan(&running); err != nil {
		return out, err
	}
	if running > 0 {
		return out, fmt.Errorf("%w: %d documents of the repository are still being finished; try again when they are done", domain.ErrConflict, running)
	}
	removed, err := tx.Exec(ctx, `DELETE FROM doc_finalization_jobs WHERE repo_id=$1`, string(request.From))
	if err != nil {
		return out, err
	}
	out.RemovedJobs = removed.RowsAffected()

	tables, err := repoAddressTables(ctx, tx)
	if err != nil {
		return out, err
	}
	var searched, kept []string
	for table, columns := range tables {
		switch {
		case repoAddressContent(table, columns):
		case repoAddressHistory[table]:
			kept = append(kept, table)
		default:
			searched = append(searched, table)
		}
	}
	sort.Strings(searched)
	sort.Strings(kept)
	found, err := repoAddressSearch(ctx, tx, tables, searched, request.From)
	if err != nil {
		return out, err
	}
	changes := map[string][]repoAddressColumn{}
	var stuck []string
	for _, hit := range found {
		name := hit.table + "." + hit.column.name
		column := domain.RepoAddressColumn{Table: hit.table, Column: hit.column.name, Rows: hit.rows}
		switch {
		case !hit.column.rewritable():
			stuck = append(stuck, fmt.Sprintf("%s (%s, %d rows)", name, hit.column.kind, hit.rows))
		default:
			changes[hit.table] = append(changes[hit.table], hit.column)
			out.Changed = append(out.Changed, column)
		}
	}
	if len(stuck) > 0 {
		return out, fmt.Errorf("%w: the old identifier is in columns that cannot be rewritten: %s", domain.ErrConflict, strings.Join(stuck, ", "))
	}
	history, err := repoAddressSearch(ctx, tx, tables, kept, request.From)
	if err != nil {
		return out, err
	}
	for _, hit := range history {
		out.Kept = append(out.Kept, domain.RepoAddressColumn{Table: hit.table, Column: hit.column.name, Rows: hit.rows})
	}

	changed := make([]string, 0, len(changes))
	for table := range changes {
		changed = append(changed, table)
	}
	sort.Strings(changed)
	triggered := append([]string{"repos"}, changed...)
	for _, table := range triggered {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE public.%s DISABLE TRIGGER USER`, pgx.Identifier{table}.Sanitize())); err != nil {
			return out, err
		}
	}
	// One statement: every reference is checked against the repository at
	// its end, when all of them name the new identifier.
	parts := make([]string, 0, len(changed)+1)
	for index, table := range changed {
		var sets, matches []string
		for _, column := range changes[table] {
			name := pgx.Identifier{column.name}.Sanitize()
			sets = append(sets, fmt.Sprintf("%s = CASE WHEN strpos(%s, $1) > 0 THEN %s ELSE %s END", name, column.text(), column.replaced(), name))
			matches = append(matches, fmt.Sprintf("strpos(%s, $1) > 0", column.text()))
		}
		parts = append(parts, fmt.Sprintf("c%d AS (UPDATE public.%s SET %s WHERE %s RETURNING 1)", index, pgx.Identifier{table}.Sanitize(), strings.Join(sets, ", "), strings.Join(matches, " OR ")))
	}
	parts = append(parts, `moved AS (UPDATE public.repos SET id = $2, remote_url = $3 WHERE id = $1 RETURNING 1)`)
	var moved int64
	if err := tx.QueryRow(ctx, "WITH "+strings.Join(parts, ",\n")+"\nSELECT count(*) FROM moved", string(request.From), string(request.To), request.ToURL).Scan(&moved); err != nil {
		return out, err
	}
	if moved != 1 {
		return out, fmt.Errorf("%w: the stored repository was not moved", domain.ErrConflict)
	}
	for _, table := range triggered {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE public.%s ENABLE TRIGGER USER`, pgx.Identifier{table}.Sanitize())); err != nil {
			return out, err
		}
	}
	// Nothing outside what is kept may name the old identifier any more.
	left, err := repoAddressSearch(ctx, tx, tables, searched, request.From)
	if err != nil {
		return out, err
	}
	if len(left) > 0 {
		return out, fmt.Errorf("%w: %s.%s still names the old identifier", domain.ErrIntegrity, left[0].table, left[0].column.name)
	}
	var stored, workspace string
	if err := tx.QueryRow(ctx, `SELECT remote_url, COALESCE(repository_id,'') FROM repos WHERE id=$1`, string(request.To)).Scan(&stored, &workspace); err != nil {
		return out, mapNoRows(err)
	}
	if stored != request.ToURL || workspace != request.RepositoryID {
		return out, fmt.Errorf("%w: the moved repository is not stored as asked", domain.ErrIntegrity)
	}
	if !request.Apply {
		return out, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return out, storageWriteError(err)
	}
	out.Applied = true
	return out, nil
}
