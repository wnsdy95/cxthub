//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

func (transaction *docDisplayReferencesTestTx) Query(_ context.Context, statement string, arguments ...any) (pgx.Rows, error) {
	operation := transaction.record(statement, arguments)
	if operation != "proof" || len(arguments) != 6 {
		transaction.test.Fatal("unexpected vector reference proof query", statement, arguments)
	}
	if operation == transaction.fail {
		return nil, transaction.failure
	}
	hashes := arguments[1].([]string)
	bodies := arguments[2].([][]byte)
	counts := arguments[3].([]int64)
	parents, children := arguments[4].([]string), arguments[5].([]string)
	if len(hashes) == 0 || len(hashes) > domain.MaxDocEventDisplayStepObjects || len(bodies) != len(hashes) || len(counts) != len(hashes) || len(parents) != len(children) {
		transaction.test.Fatal("unbounded or misaligned vector proof arguments", len(hashes), len(bodies), len(counts), len(parents), len(children))
	}
	size := 0
	for index, hash := range hashes {
		if len(bodies[index]) == 0 || len(bodies[index]) > domain.MaxDocDisplayPageBytes || domain.HashContent(bodies[index]) != domain.ContentHash(hash) {
			transaction.test.Fatal("vector proof lacks a bounded parent body", hash)
		}
		size += len(bodies[index])
	}
	if size > domain.MaxDocEventDisplayStepBytes {
		transaction.test.Fatal("vector proof exceeded the byte budget", size)
	}
	ordered := slices.Clone(hashes)
	slices.Sort(ordered)
	transaction.rows = &docDisplayReferencesTestRows{test: transaction.test, scanFailure: transaction.scanFailure, failure: transaction.rowsFailure}
	for index, hash := range ordered {
		if transaction.missing && index == len(ordered)-1 {
			continue
		}
		proof := docDisplayReferencesTestProof{hash: hash, agrees: transaction.disagree == ""}
		if transaction.replay {
			proof.version, proof.covered = 1, !transaction.inexact
		}
		if override, exists := transaction.proofs[hash]; exists {
			proof = override
			proof.hash = hash
		}
		transaction.rows.proofs = append(transaction.rows.proofs, proof)
	}
	return transaction.rows, nil
}

func TestDocDisplayReferencesPutAtomicReplay(test *testing.T) {
	source := domain.HashContent([]byte("source"))
	first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
	raw := []byte(fmt.Sprintf(`{"first":"%s","nested":["%s","%s"]}`, first, second, first))
	object := domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
	wantChildren := []string{string(first), string(second)}
	slices.Sort(wantChildren)
	for _, replay := range []bool{false, true} {
		test.Run(fmt.Sprint(replay), func(test *testing.T) {
			transaction := &docDisplayReferencesTestTx{test: test, replay: replay, coverageRows: 1}
			if err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{object}); err != nil {
				test.Fatal(err)
			}
			wantCalls := []string{"queue", "object", "source", "proof", "edges", "coverage"}
			if replay {
				wantCalls = []string{"queue", "object", "source", "proof"}
			}
			if !reflect.DeepEqual(transaction.calls, wantCalls) {
				test.Fatal("publication escaped source-first atomic ordering", transaction.calls)
			}
			hashes, bodies, counts := []string{string(object.Hash)}, [][]byte{raw}, []int64{2}
			parents := []string{string(object.Hash), string(object.Hash)}
			if !reflect.DeepEqual(transaction.arguments["object"], []any{source, hashes, bodies}) ||
				!reflect.DeepEqual(transaction.arguments["proof"], []any{source, hashes, bodies, counts, parents, wantChildren}) {
				test.Fatal("vector publication lost a parent byte or full-tuple candidate proof", transaction.arguments)
			}
			if !replay && (!reflect.DeepEqual(transaction.arguments["edges"], []any{source, parents, wantChildren}) ||
				!reflect.DeepEqual(transaction.arguments["coverage"], []any{source, hashes, counts})) {
				test.Fatal("candidate edges lost absent digests, deduplication, or exact parent counts", transaction.arguments)
			}
			if !transaction.rows.closed || !bytes.Equal(object.Bytes, raw) {
				test.Fatal("reference indexing changed immutable object bytes")
			}
		})
	}
}

func TestDocDisplayReferencesEmptyPutStillQueues(test *testing.T) {
	transaction := &docDisplayReferencesTestTx{test: test}
	if err := putDocDisplayObjects(context.Background(), transaction, domain.HashContent([]byte("source")), nil); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(transaction.calls, []string{"queue"}) {
		test.Fatal("empty publication missed the source revision fence", transaction.calls)
	}
}

func TestDocDisplayReferencesRejectUnprovableInput(test *testing.T) {
	source := domain.HashContent([]byte("source"))
	for _, raw := range [][]byte{nil, []byte("legacy bytes"), []byte(`{} []`), []byte(`{"missing":`), bytes.Repeat([]byte("x"), domain.MaxDocDisplayPageBytes+1)} {
		transaction := &docDisplayReferencesTestTx{test: test, coverageRows: 1}
		if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, domain.HashContent(raw), raw); err == nil {
			test.Fatal("unsupported bytes acquired reference coverage", string(raw[:min(len(raw), 32)]))
		}
		if len(transaction.calls) != 0 {
			test.Fatal("unsupported bytes changed indexing state", transaction.calls)
		}
	}
	for _, hashes := range [][2]domain.ContentHash{{"invalid", domain.HashContent([]byte(`{}`))}, {source, "invalid"}, {source, domain.HashContent([]byte("other"))}} {
		transaction := &docDisplayReferencesTestTx{test: test}
		if err := indexDocDisplayObjectReferences(context.Background(), transaction, hashes[0], hashes[1], []byte(`{}`)); !errors.Is(err, domain.ErrIntegrity) || len(transaction.calls) != 0 {
			test.Fatal("invalid source/hash proof touched storage", err, transaction.calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transaction := &docDisplayReferencesTestTx{test: test}
	if err := putDocDisplayObjects(ctx, transaction, source, nil); !errors.Is(err, context.Canceled) || len(transaction.calls) != 0 {
		test.Fatal("cancelled publication touched storage", err, transaction.calls)
	}
	if err := indexDocDisplayObjectReferences(ctx, transaction, source, domain.HashContent([]byte(`{}`)), []byte(`{}`)); !errors.Is(err, context.Canceled) || len(transaction.calls) != 0 {
		test.Fatal("cancelled index touched storage", err, transaction.calls)
	}
}

func TestDocDisplayReferencesRejectIncompleteProof(test *testing.T) {
	source := domain.HashContent([]byte("source"))
	raw := []byte(`null`)
	hash := domain.HashContent(raw)
	transaction := &docDisplayReferencesTestTx{test: test, fail: "source", failure: pgx.ErrNoRows, coverageRows: 1}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") {
		test.Fatal("missing source acquired coverage", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test, missing: true, coverageRows: 1}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") || !transaction.rows.closed {
		test.Fatal("missing requested object acquired coverage", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test, disagree: "proof", coverageRows: 1}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") || !transaction.rows.closed {
		test.Fatal("corrupt stored bytes acquired coverage", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test, replay: true, disagree: "replay", coverageRows: 1}
	if err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{{Hash: hash, Bytes: raw}}); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") || !transaction.rows.closed {
		test.Fatal("replay bypassed the existing byte proof", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test, replay: true, inexact: true, coverageRows: 1}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") || !transaction.rows.closed {
		test.Fatal("covered replay trusted the flag without an exact edge proof", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test, proofs: map[string]docDisplayReferencesTestProof{string(hash): {agrees: true, version: 2}}, coverageRows: 1}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) || slices.Contains(transaction.calls, "coverage") {
		test.Fatal("unknown reference coverage version was accepted", err, transaction.calls)
	}
	transaction = &docDisplayReferencesTestTx{test: test}
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, source, hash, raw); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("inexact existing edges acquired coverage", err)
	}
}

func TestDocDisplayReferencesFailureStopsPublication(test *testing.T) {
	failure := errors.New("injected reference indexing failure")
	source := domain.HashContent([]byte("source"))
	raw := []byte(fmt.Sprintf(`{"child":"%s"}`, source))
	for _, operation := range []string{"queue", "object", "source", "proof", "edges", "coverage"} {
		test.Run(operation, func(test *testing.T) {
			transaction := &docDisplayReferencesTestTx{test: test, fail: operation, failure: failure, coverageRows: 1}
			if err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{{Hash: domain.HashContent(raw), Bytes: raw}}); !errors.Is(err, failure) {
				test.Fatal("failure did not propagate to transaction owner", err)
			}
			if operation != "coverage" && slices.Contains(transaction.calls, "coverage") {
				test.Fatal("failed publication advanced coverage", transaction.calls)
			}
			if transaction.rows != nil && !transaction.rows.closed {
				test.Fatal("failed publication leaked proof rows", transaction.calls)
			}
		})
	}
	for _, operation := range []string{"scan", "iteration"} {
		test.Run(operation, func(test *testing.T) {
			transaction := &docDisplayReferencesTestTx{test: test, coverageRows: 1}
			if operation == "scan" {
				transaction.scanFailure = failure
			} else {
				transaction.rowsFailure = failure
			}
			if err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{{Hash: domain.HashContent(raw), Bytes: raw}}); !errors.Is(err, failure) ||
				slices.Contains(transaction.calls, "edges") || slices.Contains(transaction.calls, "coverage") || !transaction.rows.closed {
				test.Fatal("incomplete row proof advanced indexing or leaked rows", err, transaction.calls)
			}
		})
	}
}

func TestDocDisplayReferencesLeafCoverage(test *testing.T) {
	transaction := &docDisplayReferencesTestTx{test: test, coverageRows: 1}
	raw := []byte(`null`)
	if err := indexDocDisplayObjectReferences(context.Background(), transaction, domain.HashContent([]byte("source")), domain.HashContent(raw), raw); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(transaction.calls, []string{"source", "proof", "coverage"}) ||
		!reflect.DeepEqual(transaction.arguments["proof"][3], []int64{0}) ||
		!reflect.DeepEqual(transaction.arguments["coverage"][2], []int64{0}) ||
		len(transaction.arguments["proof"][4].([]string)) != 0 || len(transaction.arguments["proof"][5].([]string)) != 0 {
		test.Fatal("reference-free object lacks an exact empty edge witness", transaction.calls, transaction.arguments)
	}
}

func TestDocDisplayReferencesMixedCoverage(test *testing.T) {
	source := domain.HashContent([]byte("source"))
	coveredRaw := []byte(fmt.Sprintf(`{"covered":"%s"}`, domain.HashContent([]byte("covered child"))))
	pendingChild := domain.HashContent([]byte("pending child"))
	pendingRaw := []byte(fmt.Sprintf(`{"pending":"%s"}`, pendingChild))
	covered := domain.DocRangeObject{Hash: domain.HashContent(coveredRaw), Bytes: coveredRaw}
	pending := domain.DocRangeObject{Hash: domain.HashContent(pendingRaw), Bytes: pendingRaw}
	transaction := &docDisplayReferencesTestTx{test: test, proofs: map[string]docDisplayReferencesTestProof{
		string(covered.Hash): {agrees: true, version: 1, covered: true},
	}, coverageRows: 1}
	if err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{covered, pending, covered}); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(transaction.arguments["edges"], []any{source, []string{string(pending.Hash)}, []string{string(pendingChild)}}) ||
		!reflect.DeepEqual(transaction.arguments["coverage"], []any{source, []string{string(pending.Hash)}, []int64{1}}) ||
		len(transaction.arguments["proof"][1].([]string)) != 2 {
		test.Fatal("mixed replay rewrote covered parents or lost pending parents", transaction.arguments)
	}
}

func TestDocDisplayReferencesRejectPartialBatchCoverage(test *testing.T) {
	source := domain.HashContent([]byte("source"))
	first, second := []byte(`null`), []byte(`{}`)
	transaction := &docDisplayReferencesTestTx{test: test, coverageRows: 1}
	err := putDocDisplayObjects(context.Background(), transaction, source, []domain.DocRangeObject{
		{Hash: domain.HashContent(first), Bytes: first}, {Hash: domain.HashContent(second), Bytes: second},
	})
	if !errors.Is(err, domain.ErrIntegrity) || len(transaction.arguments["coverage"][1].([]string)) != 2 {
		test.Fatal("partial coverage update was accepted", err, transaction.arguments)
	}
}

func TestDocDisplayReferencesBatchPreparationBounds(test *testing.T) {
	test.Run("parents", func(test *testing.T) {
		objects := make([]domain.DocRangeObject, domain.MaxDocEventDisplayStepObjects+1)
		for index := range objects {
			raw := []byte(fmt.Sprintf(`{"index":%d}`, index))
			objects[index] = domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
		}
		batch, consumed, err := prepareDocDisplayObjectBatch(context.Background(), objects)
		if err != nil || consumed != domain.MaxDocEventDisplayStepObjects || len(batch.hashes) != consumed || slices.Contains(batch.hashes, string(objects[consumed].Hash)) {
			test.Fatal("parent batch exceeded its bound or lost its continuation", consumed, len(batch.hashes), err)
		}
	})
	test.Run("bytes", func(test *testing.T) {
		limit := domain.MaxDocEventDisplayStepBytes / domain.MaxDocDisplayPageBytes
		objects := make([]domain.DocRangeObject, limit+1)
		for index := range objects {
			raw := []byte(fmt.Sprintf(`"%d%s"`, index, strings.Repeat("x", domain.MaxDocDisplayPageBytes-3)))
			objects[index] = domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
		}
		batch, consumed, err := prepareDocDisplayObjectBatch(context.Background(), objects)
		if err != nil || consumed != limit || len(batch.hashes) != limit {
			test.Fatal("byte batch exceeded 1 MiB or stopped before the exact boundary", consumed, len(batch.hashes), err)
		}
		batch, consumed, err = prepareDocDisplayObjectBatch(context.Background(), objects[consumed:])
		if err != nil || consumed != 1 || len(batch.hashes) != 1 {
			test.Fatal("byte-bounded preparation lost the next batch", consumed, len(batch.hashes), err)
		}
		for index := range objects {
			objects[index] = objects[0]
		}
		batch, consumed, err = prepareDocDisplayObjectBatch(context.Background(), objects)
		if err != nil || consumed != limit || len(batch.hashes) != 1 {
			test.Fatal("duplicate inputs bypassed the byte budget", consumed, len(batch.hashes), err)
		}
	})
	test.Run("deduplication", func(test *testing.T) {
		child := domain.HashContent([]byte("child"))
		raw := []byte(fmt.Sprintf(`{"children":["%s","%s"]}`, child, child))
		object := domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
		batch, consumed, err := prepareDocDisplayObjectBatch(context.Background(), []domain.DocRangeObject{object, object})
		if err != nil || consumed != 2 || !reflect.DeepEqual(batch.hashes, []string{string(object.Hash)}) ||
			!reflect.DeepEqual(batch.bodies, [][]byte{raw}) || !reflect.DeepEqual(batch.counts, []int64{1}) ||
			!reflect.DeepEqual(batch.parents, []string{string(object.Hash)}) || !reflect.DeepEqual(batch.children, []string{string(child)}) {
			test.Fatal("duplicate parents or children distorted the expected relation", consumed, batch, err)
		}
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
