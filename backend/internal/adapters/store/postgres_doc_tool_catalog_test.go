//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docToolKeyCatalogPGRead struct {
	name     string
	catalogs int
	call     func(*testing.T, context.Context, *postgresDocEventDisplayWork) error
}

func docToolKeyCatalogPGReads(tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, orders domain.DocToolOrdersSource, object, array domain.DocToolOrderSource) []docToolKeyCatalogPGRead {
	return []docToolKeyCatalogPGRead{
		{"members", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			rows, err := reader.ReadToolKeyMembers(read, tree, keys, tree.Root.From, nil, 1)
			if err != nil && rows != nil || err == nil && len(rows) != 1 {
				test.Fatal("member read returned partial or missing data", rows, err)
			}
			return err
		}},
		{"order_nodes", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			rows, err := readDocToolOrderNodes(read, reader.storage.db(read), reader.source, tree, keys, tree.Root.From-1, 1)
			if err != nil && rows != nil || err == nil && (len(rows) != 1 || rows[0].Value.From != tree.Root.From) {
				test.Fatal("order node read returned partial or missing data", rows, err)
			}
			return err
		}},
		{"object_source", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			got, err := reader.ReadToolOrderSource(read, tree, keys, object.Parent.Value.From)
			if err != nil && !reflect.DeepEqual(got, domain.DocToolOrderSource{}) || err == nil && !equalDocToolOrderSource(tree, keys, got, object) {
				test.Fatal("object source read returned partial or changed data", got, err)
			}
			return err
		}},
		{"array_source", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			got, err := reader.ReadToolOrderSource(read, tree, keys, array.Parent.Value.From)
			if err != nil && !reflect.DeepEqual(got, domain.DocToolOrderSource{}) || err == nil && !equalDocToolOrderSource(tree, keys, got, array) {
				test.Fatal("array source read returned partial or changed data", got, err)
			}
			return err
		}},
		{"object_entries", 2, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			rows, err := reader.ReadToolOrderEntries(read, tree, keys, object, nil, 1)
			if err != nil && rows != nil || err == nil && len(rows) != 1 {
				test.Fatal("object entries returned partial or missing data", rows, err)
			}
			return err
		}},
		{"array_entries", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			rows, err := reader.ReadToolOrderEntries(read, tree, keys, array, nil, 1)
			if err != nil && rows != nil || err == nil && len(rows) != 1 {
				test.Fatal("array entries returned partial or missing data", rows, err)
			}
			return err
		}},
		{"order_plan", 1, func(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) error {
			got, err := reader.ReadToolOrderPlan(read, tree, keys, orders, tree.Root.From)
			if err != nil && !reflect.DeepEqual(got, domain.DocToolOrderPlan{}) || err == nil && !equalDocToolOrderSource(tree, keys, got.Source, object) {
				test.Fatal("order plan returned partial or changed data", got, err)
			}
			return err
		}},
	}
}

func assertDocToolKeyCatalogPGTrace(test *testing.T, trace *docToolOrdersPGTrace, query string, arguments []any, count int) {
	test.Helper()
	if len(trace.queries) != len(trace.arguments) {
		test.Fatal("catalog trace lost arguments")
	}
	seen := 0
	for index, actual := range trace.queries {
		if !strings.Contains(actual, "doc_tool_tree_sources_v1") && !strings.Contains(actual, "doc_tool_key_sources_v1") {
			continue
		}
		if actual != query || !reflect.DeepEqual(trace.arguments[index], arguments) {
			test.Fatal("reader split or changed the current header check", actual, trace.arguments[index])
		}
		seen++
	}
	if seen != count {
		test.Fatal("reader omitted or repeated a catalog check", seen, count, trace.queries)
	}
}

func dropDocToolKeyCatalogPGChecks(test *testing.T, ctx context.Context, transaction pgx.Tx) {
	test.Helper()
	rows, err := transaction.Query(ctx, `SELECT relation.relname,stored.conname
 FROM pg_constraint stored JOIN pg_class relation ON relation.oid=stored.conrelid
 WHERE stored.contype='c' AND stored.conrelid IN
 ('doc_tool_tree_sources_v1'::regclass,'doc_tool_key_sources_v1'::regclass)
 ORDER BY relation.relname,stored.conname`)
	if err != nil {
		test.Fatal(err)
	}
	var constraints [][2]string
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			rows.Close()
			test.Fatal(err)
		}
		constraints = append(constraints, [2]string{table, name})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	if len(constraints) == 0 {
		test.Fatal("catalog corruption fixture found no header checks")
	}
	for _, constraint := range constraints {
		if _, err := transaction.Exec(ctx, `ALTER TABLE `+pgx.Identifier{constraint[0]}.Sanitize()+
			` DROP CONSTRAINT `+pgx.Identifier{constraint[1]}.Sanitize()); err != nil {
			test.Fatal(err)
		}
	}
}

func TestDocToolKeyCatalogPG(test *testing.T) {
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": []any{true, false}, "b": 2})
	prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
	tree, keys, orders := *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders
	owner, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	keyScope, err := keys.Hash(tree)
	if err != nil {
		test.Fatal(err)
	}
	keyRaw, err := keys.Encode(tree)
	if err != nil {
		test.Fatal(err)
	}
	var object, array domain.DocToolOrderSource
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		object, err = reader.ReadToolOrderSource(read, tree, keys, tree.Root.From)
		if err != nil {
			test.Fatal(err)
		}
		nodes, err := readDocToolOrderNodes(read, fixture.storage.db(read), owner, tree, keys, tree.Root.From-1, domain.DocToolKeyMapStepNodes)
		if err != nil || len(nodes) != keys.Nodes {
			test.Fatal("small catalog fixture lost its nodes", len(nodes), err)
		}
		for _, node := range nodes {
			if node.Type == domain.DocToolValueArray {
				array, err = reader.ReadToolOrderSource(read, tree, keys, node.Value.From)
				if err != nil {
					test.Fatal(err)
				}
			}
		}
	})
	if object.Count != 2 || array.Count != 2 {
		test.Fatal("catalog fixture needs object and array paths", object.Count, array.Count)
	}
	reads := docToolKeyCatalogPGReads(tree, keys, orders, object, array)
	before := docToolOrdersPGImage(test, fixture, job)
	var catalogQuery string
	catalogArguments := []any{keys.Tree, keyScope, domain.MaxDocDisplaySourceBytes + 1}

	test.Run("OneQueryAndPlans", func(test *testing.T) {
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			original := read.Value(repositoryTxKey{}).(*repositoryTx)
			trace := &docToolOrdersPGTrace{Tx: original.Tx}
			if err := validateDocToolKeyCatalog(read, trace, owner, tree, keys); err != nil {
				return err
			}
			if len(trace.queries) != 1 || trace.rows != 0 || trace.objects != 0 {
				test.Fatal("catalog validation must make exactly one real header query", trace.queries, trace.rows, trace.objects)
			}
			catalogQuery = trace.queries[0]
			query := strings.Join(strings.Fields(catalogQuery), " ")
			if !strings.Contains(query, "FROM doc_tool_tree_sources_v1 tree LEFT JOIN doc_tool_key_sources_v1 keys ON keys.scope=$2 WHERE tree.scope=$1") ||
				!strings.Contains(query, "substring(tree.bytes FROM 1 FOR $3)") || !strings.Contains(query, "substring(keys.bytes FROM 1 FOR $3)") {
				test.Fatal("catalog query lost its two exact identities or bounded raw projections", query)
			}
			assertDocToolKeyCatalogPGTrace(test, trace, catalogQuery, catalogArguments, 1)
			for _, sequential := range []string{"on", "off"} {
				if _, err := original.Tx.Exec(read, `SET LOCAL enable_seqscan=`+sequential); err != nil {
					return err
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					encoded := explainDocToolOrdersPGTraceQuery(test, read, original.Tx, catalogQuery, trace.arguments[0], mode)
					blocks := assertDocToolOrdersPGPointPlan(test, encoded, mode+"/seqscan="+sequential, 0, catalogQuery)
					if sequential == "off" && (!strings.Contains(string(encoded), "doc_tool_tree_sources_v1_pkey") ||
						!strings.Contains(string(encoded), "doc_tool_key_sources_v1_pkey") && !strings.Contains(string(encoded), "doc_tool_key_sources_v1_scope_tree_scope_key")) {
						test.Fatal("catalog plan did not probe both unique scope indexes", string(encoded))
					}
					test.Logf("plan=%s seqscan=%s queries=1 blocks=%.0f no_header_filler=true", mode, sequential, blocks)
				}
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})
	if catalogQuery == "" {
		test.Fatal("catalog query was not captured")
	}

	test.Run("ReadersUseJoinedCatalog", func(test *testing.T) {
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			original := read.Value(repositoryTxKey{}).(*repositoryTx)
			trace := &docToolOrdersPGTrace{Tx: original.Tx}
			bound := *original
			bound.Tx = trace
			read = context.WithValue(read, repositoryTxKey{}, &bound)
			reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: &bound, source: owner, tree: &tree, keys: &keys, orders: &orders}
			for _, selected := range reads {
				test.Run(selected.name, func(test *testing.T) {
					trace.queries, trace.arguments = nil, nil
					if err := selected.call(test, read, reader); err != nil {
						test.Fatal(err)
					}
					assertDocToolKeyCatalogPGTrace(test, trace, catalogQuery, catalogArguments, selected.catalogs)
				})
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})

	test.Run("InvalidInputs", func(test *testing.T) {
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			transaction := read.Value(repositoryTxKey{}).(*repositoryTx)
			for _, name := range []string{"owner", "tree", "key_tree", "key_profile", "node_count_zero", "node_count_bounds", "node_count_mismatch", "wrong_owner"} {
				test.Run(name, func(test *testing.T) {
					wrongOwner, wrongTree, wrongKeys, queries := owner, tree, keys, 0
					switch name {
					case "owner":
						wrongOwner = ""
					case "tree":
						wrongTree.Root.To = wrongTree.Root.From
					case "key_tree":
						wrongKeys.Tree = domain.HashContent([]byte("other catalog tree"))
					case "key_profile":
						wrongKeys.Profile += "x"
					case "node_count_zero":
						wrongKeys.Nodes = 0
					case "node_count_bounds":
						wrongKeys.Nodes = tree.Root.To - tree.Root.From + 1
					case "node_count_mismatch":
						wrongKeys.Nodes++
						queries = 1
					case "wrong_owner":
						wrongOwner = domain.HashContent([]byte("other catalog owner"))
						queries = 1
					}
					for _, validate := range []func(context.Context, interface {
						QueryRow(context.Context, string, ...any) pgx.Row
					}, domain.ContentHash, domain.DocToolTreeSource, domain.DocToolKeyMapSource) error{validateDocToolKeyTree, validateDocToolKeyCatalog} {
						trace := &docToolOrdersPGTrace{Tx: transaction.Tx}
						if err := validate(read, trace, wrongOwner, wrongTree, wrongKeys); !errors.Is(err, domain.ErrIntegrity) || len(trace.queries) != queries {
							test.Fatal("invalid input changed validation or issued unnecessary SQL", err, trace.queries, queries)
						}
					}
				})
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})

	test.Run("CurrentHeadersAndErrorPrecedence", func(test *testing.T) {
		var members int
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT member_count FROM doc_tool_key_sources_v1 WHERE scope=$1`, keyScope).Scan(&members); err != nil {
			test.Fatal(err)
		}
		docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{"doc_tool_key_sources_v1"},
			`UPDATE doc_tool_key_sources_v1 SET state='scanning',member_count=NULL WHERE scope=$1`, keyScope)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := fixture.storage.docDisplayWrite(cleanup, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
				if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
					return err
				}
				_, err := transaction.Exec(bound, `UPDATE doc_tool_key_sources_v1 SET state='complete',member_count=$2 WHERE scope=$1`, keyScope, members)
				return err
			}); err != nil {
				test.Error("restore original completed header", err)
			}
		}()
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		dropDocToolKeyCatalogPGChecks(test, fixture.ctx, transaction)
		otherTree := domain.HashContent([]byte(test.Name() + "/tree"))
		otherOwner := domain.HashContent([]byte(test.Name() + "/owner"))
		setDocToolCatalogPGWriterGuards(test, fixture, transaction, []string{"doc_tool_tree_sources_v1"}, false)
		if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_tool_tree_sources_v1(scope,source_hash,bytes,root_from,root_to)
 SELECT $1,source_hash,bytes,root_from,root_to FROM doc_tool_tree_sources_v1 WHERE scope=$2`, otherTree, keys.Tree); err != nil {
			test.Fatal(err)
		}
		setDocToolCatalogPGWriterGuards(test, fixture, transaction, []string{"doc_tool_tree_sources_v1"}, true)
		if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 SELECT $1,profile,$1,bytes FROM doc_display_sources_v1 WHERE source_hash=$2`, otherOwner, owner); err != nil {
			test.Fatal(err)
		}
		for _, mutation := range []struct {
			name     string
			treeSet  string
			keySet   string
			remove   string
			rehome   bool
			want     error
			treeWant error
		}{
			{name: "complete"},
			{name: "key_count_lower_bound", keySet: "state='complete',member_count=0"},
			{name: "key_count_upper_bound", keySet: "state='complete',member_count=" + docToolOrdersPGPlanArguments(test, []any{keys.Nodes})},
			{name: "tree_owner", treeSet: "source_hash='" + string(otherOwner) + "'", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_raw", treeSet: "bytes=bytes||decode('20','hex')", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_raw_oversized", treeSet: "bytes=bytes||decode(repeat('20',4096),'hex')", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_count", treeSet: "node_count=node_count+1", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_count_negative", treeSet: "node_count=-1", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_from", treeSet: "root_from=root_from+1", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_to", treeSet: "root_to=root_to-1", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_incomplete", treeSet: "state='scanning',node_count=NULL", want: domain.ErrDocRangeIndexUnavailable, treeWant: domain.ErrDocRangeIndexUnavailable},
			{name: "tree_complete_without_count", treeSet: "node_count=NULL", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_scanning_with_count", treeSet: "state='scanning'", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "tree_unknown_state_retains_unavailable", treeSet: "state='unknown',node_count=NULL", want: domain.ErrDocRangeIndexUnavailable, treeWant: domain.ErrDocRangeIndexUnavailable},
			{name: "key_tree", rehome: true, want: domain.ErrIntegrity},
			{name: "key_raw", keySet: "state='complete',member_count=$2,bytes=bytes||decode('20','hex')", want: domain.ErrIntegrity},
			{name: "key_raw_oversized", keySet: "state='complete',member_count=$2,bytes=bytes||decode(repeat('20',4096),'hex')", want: domain.ErrIntegrity},
			{name: "key_unknown_state", keySet: "state='unknown',member_count=NULL", want: domain.ErrIntegrity},
			{name: "key_count_negative", keySet: "state='complete',member_count=-1", want: domain.ErrIntegrity},
			{name: "key_count_over_nodes", keySet: "state='complete',member_count=" + docToolOrdersPGPlanArguments(test, []any{keys.Nodes + 1}), want: domain.ErrIntegrity},
			{name: "key_complete_without_count", keySet: "state='complete',member_count=NULL", want: domain.ErrIntegrity},
			{name: "key_scanning_with_count", keySet: "state='scanning',member_count=$2", want: domain.ErrIntegrity},
			{name: "key_incomplete", keySet: "state='scanning',member_count=NULL", want: domain.ErrDocRangeIndexUnavailable},
			{name: "missing_tree", remove: "doc_tool_tree_sources_v1", want: domain.ErrNotFound, treeWant: domain.ErrNotFound},
			{name: "missing_keys", remove: "doc_tool_key_sources_v1", want: domain.ErrNotFound},
			{name: "both_incomplete", treeSet: "state='scanning',node_count=NULL", keySet: "state='scanning',member_count=NULL", want: domain.ErrDocRangeIndexUnavailable, treeWant: domain.ErrDocRangeIndexUnavailable},
			{name: "incomplete_tree_before_missing_keys", treeSet: "state='scanning',node_count=NULL", remove: "doc_tool_key_sources_v1", want: domain.ErrDocRangeIndexUnavailable, treeWant: domain.ErrDocRangeIndexUnavailable},
			{name: "incomplete_tree_before_corrupt_keys", treeSet: "state='scanning',node_count=NULL", keySet: "state='unknown',member_count=NULL", want: domain.ErrDocRangeIndexUnavailable, treeWant: domain.ErrDocRangeIndexUnavailable},
			{name: "corrupt_tree_before_missing_keys", treeSet: "bytes=bytes||decode('20','hex')", remove: "doc_tool_key_sources_v1", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "corrupt_tree_before_incomplete_keys", treeSet: "node_count=node_count+1", keySet: "state='scanning',member_count=NULL", want: domain.ErrIntegrity, treeWant: domain.ErrIntegrity},
			{name: "missing_tree_before_incomplete_keys", keySet: "state='scanning',member_count=NULL", remove: "doc_tool_tree_sources_v1", want: domain.ErrNotFound, treeWant: domain.ErrNotFound},
		} {
			test.Run(mutation.name, func(test *testing.T) {
				savepoint, err := transaction.Begin(fixture.ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(savepoint)
				tables := []string{"doc_tool_tree_sources_v1", "doc_tool_key_sources_v1"}
				setDocToolCatalogPGWriterGuards(test, fixture, savepoint, tables, false)
				setDocToolCatalogPGDeleteGuards(test, fixture, savepoint, docCatalogWriterTablesPG, false)
				if mutation.name == "key_raw" || mutation.name == "key_raw_oversized" {
					if _, err := savepoint.Exec(fixture.ctx, `ALTER TABLE doc_tool_key_sources_v1 DISABLE TRIGGER doc_tool_key_sources_v1_header_guard`); err != nil {
						test.Fatal(err)
					}
				}
				if mutation.rehome {
					if _, err := savepoint.Exec(fixture.ctx, `DELETE FROM doc_tool_key_sources_v1 WHERE scope=$1`, keyScope); err != nil {
						test.Fatal(err)
					}
					if _, err := savepoint.Exec(fixture.ctx, `INSERT INTO doc_tool_key_sources_v1(scope,tree_scope,bytes) VALUES($1,$2,$3)`, keyScope, otherTree, keyRaw); err != nil {
						test.Fatal(err)
					}
				}
				keySet := mutation.keySet
				if keySet == "" {
					keySet = "state='complete',member_count=$2"
				}
				arguments := []any{keyScope}
				if strings.Contains(keySet, "$2") {
					arguments = append(arguments, members)
				}
				if _, err := savepoint.Exec(fixture.ctx, `UPDATE doc_tool_key_sources_v1 SET `+keySet+` WHERE scope=$1`, arguments...); err != nil {
					test.Fatal(err)
				}
				if mutation.treeSet != "" {
					if _, err := savepoint.Exec(fixture.ctx, `UPDATE doc_tool_tree_sources_v1 SET `+mutation.treeSet+` WHERE scope=$1`, keys.Tree); err != nil {
						test.Fatal(err)
					}
				}
				if mutation.remove != "" {
					scope := keyScope
					if mutation.remove == "doc_tool_tree_sources_v1" {
						scope = keys.Tree
					}
					if _, err := savepoint.Exec(fixture.ctx, `DELETE FROM `+pgx.Identifier{mutation.remove}.Sanitize()+` WHERE scope=$1`, scope); err != nil {
						test.Fatal(err)
					}
				}
				setDocToolCatalogPGWriterGuards(test, fixture, savepoint, tables, true)
				setDocToolCatalogPGDeleteGuards(test, fixture, savepoint, docCatalogWriterTablesPG, true)
				if mutation.name == "key_raw" || mutation.name == "key_raw_oversized" {
					if _, err := savepoint.Exec(fixture.ctx, `ALTER TABLE doc_tool_key_sources_v1 ENABLE TRIGGER doc_tool_key_sources_v1_header_guard`); err != nil {
						test.Fatal(err)
					}
				}
				trace := &docToolOrdersPGTrace{Tx: savepoint}
				if err := validateDocToolKeyTree(fixture.ctx, trace, owner, tree, keys); !errors.Is(err, mutation.treeWant) || len(trace.queries) != 1 {
					test.Fatal("shared tree validation changed its existing error", err, mutation.treeWant, trace.queries)
				}
				trace.queries, trace.arguments = nil, nil
				if err := validateDocToolKeyCatalog(fixture.ctx, trace, owner, tree, keys); !errors.Is(err, mutation.want) || len(trace.queries) != 1 {
					test.Fatal("joined catalog changed validation or error precedence", err, mutation.want, trace.queries)
				}
				assertDocToolKeyCatalogPGTrace(test, trace, catalogQuery, catalogArguments, 1)
				bound := &repositoryTx{Tx: trace, owner: fixture.storage, readOnly: true}
				read := context.WithValue(fixture.ctx, repositoryTxKey{}, bound)
				reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: bound, source: owner, tree: &tree, keys: &keys, orders: &orders}
				for _, selected := range reads {
					trace.queries, trace.arguments = nil, nil
					if err := selected.call(test, read, reader); !errors.Is(err, mutation.want) {
						test.Fatal("reader bypassed current headers", selected.name, err, mutation.want)
					}
					count := selected.catalogs
					if mutation.want != nil {
						count = 1
						if len(trace.queries) != 1 {
							test.Fatal("invalid catalog reached body/page queries", selected.name, trace.queries)
						}
					}
					assertDocToolKeyCatalogPGTrace(test, trace, catalogQuery, catalogArguments, count)
				}
			})
		}
	})

	test.Run("CanceledDeadlineAndClosedSnapshot", func(test *testing.T) {
		var escaped *postgresDocEventDisplayWork
		var escapedContext context.Context
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
			original := read.Value(repositoryTxKey{}).(*repositoryTx)
			trace := &docToolOrdersPGTrace{Tx: original.Tx}
			bound := *original
			bound.Tx = trace
			read = context.WithValue(read, repositoryTxKey{}, &bound)
			reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: &bound, source: owner, tree: &tree, keys: &keys, orders: &orders}
			escaped, escapedContext = reader, read
			canceled, cancel := context.WithCancel(read)
			cancel()
			expired, expire := context.WithDeadline(read, time.Now().Add(-time.Second))
			defer expire()
			for _, selected := range reads {
				if selected.name == "order_nodes" {
					continue
				}
				for _, stopped := range []struct {
					ctx  context.Context
					want error
				}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}} {
					if err := selected.call(test, stopped.ctx, reader); !errors.Is(err, stopped.want) {
						test.Fatal("stopped reader returned data or changed context error", selected.name, err, stopped.want)
					}
				}
			}
			if len(trace.queries) != 0 {
				test.Fatal("stopped bound readers issued SQL", trace.queries)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		if err := validateDocToolKeyCatalog(escapedContext, escaped.storage.db(escapedContext), owner, tree, keys); !errors.Is(err, pgx.ErrTxClosed) {
			test.Fatal("closed catalog transaction was replaced by a pool read", err)
		}
		for _, selected := range reads {
			if err := selected.call(test, escapedContext, escaped); !errors.Is(err, pgx.ErrTxClosed) {
				test.Fatal("closed snapshot returned private data", selected.name, err)
			}
			if selected.name != "order_nodes" {
				if err := selected.call(test, fixture.ctx, escaped); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("reader escaped its snapshot", selected.name, err)
				}
			}
		}
		for _, deadline := range []bool{false, true} {
			transaction, err := fixture.storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				test.Fatal(err)
			}
			func() {
				defer rollbackPG(transaction)
				read := fixture.ctx
				var cancel context.CancelFunc
				want := context.Canceled
				if deadline {
					read, cancel = context.WithDeadline(read, time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				} else {
					read, cancel = context.WithCancel(read)
				}
				cancel()
				trace := &docToolOrdersPGTrace{Tx: transaction}
				if err := validateDocToolKeyCatalog(read, trace, owner, tree, keys); !errors.Is(err, want) || len(trace.queries) != 1 {
					test.Fatal("stopped catalog returned success or hid cancellation", err, want, trace.queries)
				}
			}()
		}
	})

	test.Run("AbortedTransaction", func(test *testing.T) {
		transaction, err := fixture.storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		_, err = transaction.Exec(fixture.ctx, `SELECT 1/0`)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "22012" {
			test.Fatal("fixture did not abort the real transaction", err)
		}
		trace := &docToolOrdersPGTrace{Tx: transaction}
		bound := &repositoryTx{Tx: trace, owner: fixture.storage, readOnly: true}
		read := context.WithValue(fixture.ctx, repositoryTxKey{}, bound)
		reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: bound, source: owner, tree: &tree, keys: &keys, orders: &orders}
		if err := validateDocToolKeyCatalog(read, trace, owner, tree, keys); !errors.As(err, &failure) || failure.Code != "25P02" {
			test.Fatal("catalog hid an aborted transaction", err)
		}
		for _, selected := range reads {
			trace.queries, trace.arguments = nil, nil
			if err := selected.call(test, read, reader); !errors.As(err, &failure) || failure.Code != "25P02" || len(trace.queries) != 1 {
				test.Fatal("aborted reader returned data, hid failure or fell back", selected.name, err, trace.queries)
			}
		}
	})
	if docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("catalog tests failed to roll back their mutations")
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}
