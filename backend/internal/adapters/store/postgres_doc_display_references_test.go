//go:build postgres

package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type docDisplayReferencesTestRow func(...any) error

func (row docDisplayReferencesTestRow) Scan(destinations ...any) error {
	return row(destinations...)
}

type docDisplayReferencesTestTx struct {
	pgx.Tx
	test         *testing.T
	calls        []string
	arguments    map[string][]any
	replay       bool
	disagree     string
	inexact      bool
	missing      bool
	fail         string
	failure      error
	scanFailure  error
	rowsFailure  error
	proofs       map[string]docDisplayReferencesTestProof
	rows         *docDisplayReferencesTestRows
	coverageRows int64
}

type docDisplayReferencesTestProof struct {
	hash    string
	agrees  bool
	version int16
	covered bool
}

type docDisplayReferencesTestRows struct {
	pgx.Rows
	test        *testing.T
	proofs      []docDisplayReferencesTestProof
	position    int
	scanFailure error
	failure     error
	closed      bool
}

func (rows *docDisplayReferencesTestRows) Next() bool {
	if rows.closed || rows.position == len(rows.proofs) {
		return false
	}
	rows.position++
	return true
}

func (rows *docDisplayReferencesTestRows) Scan(destinations ...any) error {
	rows.test.Helper()
	if rows.scanFailure != nil {
		return rows.scanFailure
	}
	if len(destinations) != 4 || rows.position == 0 || rows.position > len(rows.proofs) {
		rows.test.Fatal("unexpected bounded proof row scan", rows.position, len(destinations))
	}
	proof := rows.proofs[rows.position-1]
	*destinations[0].(*string) = proof.hash
	*destinations[1].(*bool) = proof.agrees
	*destinations[2].(*int16) = proof.version
	*destinations[3].(*bool) = proof.covered
	return nil
}

func (rows *docDisplayReferencesTestRows) Close()     { rows.closed = true }
func (rows *docDisplayReferencesTestRows) Err() error { return rows.failure }

func (transaction *docDisplayReferencesTestTx) record(statement string, arguments []any) string {
	transaction.test.Helper()
	var operation string
	var required []string
	switch {
	case strings.HasPrefix(statement, "SELECT cxthub_queue_doc_display_maintenance"):
		operation = "queue"
	case strings.HasPrefix(statement, "INSERT INTO doc_display_objects_v1"):
		operation = "object"
		required = []string{
			"SELECT $1,requested.hash,requested.bytes",
			"unnest($2::text[],$3::bytea[])",
			"LEFT JOIN LATERAL (SELECT 1 AS present FROM doc_display_objects_v1 object",
			"object.source_hash=$1 AND object.hash=requested.hash LIMIT 1",
			"WHERE existing.present IS NULL ON CONFLICT DO NOTHING",
		}
	case strings.HasPrefix(statement, "SELECT 1 FROM doc_display_sources_v1"):
		operation = "source"
		required = []string{"source_hash=$1 FOR NO KEY UPDATE"}
	case strings.HasPrefix(statement, "WITH requested AS ("):
		operation = "proof"
		required = []string{
			"unnest($2::text[],$3::bytea[],$4::bigint[])",
			"unnest($5::text[],$6::text[])",
			"FROM expected CROSS JOIN LATERAL (",
			"edge.source_hash=$1 AND edge.parent_hash=expected.parent_hash AND edge.child_hash=expected.child_hash",
			"edge.child_hash=expected.child_hash LIMIT 1",
			"GROUP BY expected.parent_hash",
			"SELECT object.hash,object.agrees,object.references_version",
			"object.hash,object.bytes=requested.bytes AS agrees,object.references_version",
			"coalesce(matched.edges,0)=requested.edges",
			"edge.source_hash=$1 AND edge.parent_hash=object.hash LIMIT requested.edges+1",
			"existing_edges)=requested.edges",
			"FROM requested CROSS JOIN LATERAL (",
			"object.source_hash=$1 AND object.hash=requested.hash LIMIT 1 FOR UPDATE OF object",
			"matched.parent_hash=object.hash",
			"ORDER BY object.hash",
		}
	case strings.HasPrefix(statement, "INSERT INTO doc_display_object_edges_v1"):
		operation = "edges"
		required = []string{"SELECT $1,parent_hash,child_hash", "unnest($2::text[],$3::text[])", "ON CONFLICT DO NOTHING"}
	case strings.HasPrefix(statement, "UPDATE doc_display_objects_v1 object SET references_version=1"):
		operation = "coverage"
		required = []string{
			"unnest($2::text[],$3::bigint[])",
			"object.source_hash=$1 AND object.hash=expected.hash AND object.references_version=0",
			"edge.source_hash=$1 AND edge.parent_hash=object.hash LIMIT expected.edges+1",
			"existing_edges)=expected.edges",
		}
	default:
		transaction.test.Fatal("unexpected reference indexing query", statement)
	}
	statement = strings.Join(strings.Fields(statement), " ")
	for _, fragment := range required {
		if !strings.Contains(statement, fragment) {
			transaction.test.Fatal("reference indexing query lost a vector, lock, or exact proof", operation, fragment, statement)
		}
	}
	transaction.calls = append(transaction.calls, operation)
	if transaction.arguments == nil {
		transaction.arguments = make(map[string][]any)
	}
	transaction.arguments[operation] = arguments
	return operation
}

func (transaction *docDisplayReferencesTestTx) Exec(_ context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	operation := transaction.record(statement, arguments)
	if operation == transaction.fail {
		return pgconn.CommandTag{}, transaction.failure
	}
	if operation == "object" && transaction.replay {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	if operation == "coverage" {
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", transaction.coverageRows)), nil
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (transaction *docDisplayReferencesTestTx) QueryRow(_ context.Context, statement string, arguments ...any) pgx.Row {
	operation := transaction.record(statement, arguments)
	if operation != "source" {
		transaction.test.Fatal("unexpected scalar reference indexing query", statement)
	}
	return docDisplayReferencesTestRow(func(destinations ...any) error {
		if operation == transaction.fail {
			return transaction.failure
		}
		*destinations[0].(*int) = 1
		return nil
	})
}

func TestDocDisplayReferencesMigrationContract(test *testing.T) {
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0133_doc_display_references.sql")
	if err != nil {
		test.Fatal(err)
	}
	migration := string(raw)
	for _, required := range []string{
		"reference_revision bigint NOT NULL DEFAULT 0 CHECK (reference_revision >= 0)",
		"references_version smallint NOT NULL DEFAULT 0 CHECK (references_version IN (0,1))",
		"phase IN ('index','roots_jobs','roots_generation','roots_trees','roots_plans','mark','sweep','idle')",
		"ON doc_display_objects_v1(source_hash,hash)\n  WHERE references_version=0",
		"PRIMARY KEY (source_hash,parent_hash,child_hash)",
		"FOREIGN KEY (source_hash,parent_hash) REFERENCES doc_display_objects_v1(source_hash,hash) ON DELETE CASCADE",
		"ON doc_display_object_marks_v1(source_hash,epoch,expanded,hash)",
		"ON doc_display_jobs_v1(source_hash,repo_id,doc_hash,event_index,profile)",
		"jobs_profile text NOT NULL DEFAULT ''",
		"ON doc_tool_order_sources_v1(source_hash,scope)",
		"ON doc_tool_tree_sources_v1(source_hash,scope)",
		"BEFORE INSERT OR UPDATE OF hash,source_hash,bytes ON doc_display_objects_v1",
		"BEFORE UPDATE OF scope,tree_scope,bytes ON doc_tool_key_sources_v1",
		"(NEW.scope,NEW.tree_scope,NEW.bytes) IS DISTINCT FROM (OLD.scope,OLD.tree_scope,OLD.bytes)",
		"RAISE EXCEPTION 'tool key source headers are immutable' USING ERRCODE='23514';",
		"AFTER INSERT OR UPDATE OF repo_id,doc_hash,event_index,profile,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation OR DELETE ON doc_display_jobs_v1",
		"(NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.profile,NEW.source_hash,NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation)",
		"IS NOT DISTINCT FROM (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.profile,OLD.source_hash,OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation)",
		"AFTER INSERT OR UPDATE OR DELETE ON doc_display_generations_v1",
		"AFTER INSERT OR UPDATE OR DELETE ON doc_tool_tree_sources_v1",
		"AFTER INSERT OR UPDATE OR DELETE ON doc_tool_order_sources_v1",
		"AFTER INSERT OR UPDATE OR DELETE ON doc_tool_order_plans_v1",
		"ORDER BY roots.source_hash",
		"OLD.references_version=1 AND NEW.references_version<>1",
		"IF parent.references_version=1 THEN",
		"child_hash=NEW.child_hash",
		"(NEW.source_hash,NEW.scope,NEW.bytes) IS NOT DISTINCT FROM (OLD.source_hash,OLD.scope,OLD.bytes)",
		"(NEW.source_hash,NEW.scope,NEW.bytes,NEW.tree_scope,NEW.keys_scope)",
		"IS NOT DISTINCT FROM (OLD.source_hash,OLD.scope,OLD.bytes,OLD.tree_scope,OLD.keys_scope)",
	} {
		if !strings.Contains(migration, required) {
			test.Fatal("missing reference maintenance contract", required)
		}
	}
	edges := strings.Split(strings.Split(migration, "CREATE TABLE doc_display_object_edges_v1 (")[1], "\n);")[0]
	if strings.Count(edges, "FOREIGN KEY") != 1 || strings.Contains(edges, "REFERENCES doc_display_objects_v1(source_hash,child_hash)") {
		test.Fatal("candidate digests incorrectly require existing child objects", edges)
	}
	objectTrigger := strings.Split(strings.Split(migration, "CREATE TRIGGER doc_display_objects_v1_queue_references")[1], ";")[0]
	if strings.Contains(objectTrigger, "DELETE") || strings.Contains(objectTrigger, "references_version") {
		test.Fatal("backfill or collector deletion incorrectly requeues maintenance", objectTrigger)
	}
	objectQueue := strings.Split(strings.Split(migration, "CREATE FUNCTION cxthub_queue_doc_display_object_references()")[1], "$$;")[0]
	updateGuard := strings.Index(objectQueue, "IF TG_OP='UPDATE' THEN")
	changedObject := strings.Index(objectQueue, "(NEW.hash,NEW.source_hash,NEW.bytes) IS DISTINCT FROM (OLD.hash,OLD.source_hash,OLD.bytes)")
	rejectMutation := strings.Index(objectQueue, "RAISE EXCEPTION 'document display objects are immutable' USING ERRCODE='23514';")
	allowReplay := strings.Index(objectQueue, "RETURN NEW;")
	queueInsert := strings.Index(objectQueue, "PERFORM cxthub_queue_doc_display_maintenance(NEW.source_hash);")
	if updateGuard < 0 || changedObject <= updateGuard || rejectMutation <= changedObject || allowReplay <= rejectMutation || queueInsert <= allowReplay ||
		strings.Contains(objectQueue, "references_version") {
		test.Fatal("object mutation must fail before source fencing while exact replay and coverage-only updates remain allowed", objectQueue)
	}
	keyHeader := strings.Split(strings.Split(migration, "CREATE FUNCTION cxthub_guard_doc_tool_key_header()")[1], "$$;")[0]
	if strings.Contains(keyHeader, ".state") || !strings.Contains(keyHeader, "RETURN NEW;") {
		test.Fatal("key source headers must be immutable while scanning and allow unchanged replay", keyHeader)
	}
	treeQueue := strings.Split(strings.Split(migration, "CREATE FUNCTION cxthub_queue_doc_display_tree_roots()")[1], "$$;")[0]
	if !strings.Contains(treeQueue, "OLD.source_hash") || !strings.Contains(treeQueue, "NEW.source_hash") || !strings.Contains(treeQueue, "ORDER BY roots.source_hash") {
		test.Fatal("TREE root bytes lack sorted old/new source fencing", treeQueue)
	}
	queue := strings.Split(strings.Split(migration, "CREATE FUNCTION cxthub_queue_doc_display_maintenance(source text)")[1], "$$;")[0]
	if strings.Index(queue, "UPDATE doc_display_sources_v1") > strings.Index(queue, "INSERT INTO doc_display_maintenance_v1") ||
		!strings.Contains(queue, "WHEN doc_display_maintenance_v1.phase='idle' THEN 'index'") ||
		!strings.Contains(queue, "ELSE doc_display_maintenance_v1.phase END") ||
		!strings.Contains(queue, "LEAST(doc_display_maintenance_v1.due_at,clock_timestamp())") {
		test.Fatal("maintenance queue violates source-first revision fencing or preserves no active cursor", queue)
	}
}
