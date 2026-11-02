//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayWriteCostTrace struct {
	queries         int
	proof           string
	arguments       []any
	insert          string
	insertArguments []any
}

func (trace *docDisplayWriteCostTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries++
	if strings.HasPrefix(query.SQL, "WITH requested AS (") {
		trace.proof = query.SQL
		trace.arguments = append([]any(nil), query.Args...)
	}
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_display_objects_v1") {
		trace.insert = query.SQL
		trace.insertArguments = append([]any(nil), query.Args...)
	}
	return ctx
}

func TestDocDisplayReferenceBatchPGBoundedProofPlans(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	source, _ := maintenanceSource(test, ctx, storage, "bounded proof plans")
	hashes, bodies := make([]string, 5000), make([][]byte, 5000)
	children := make([]string, 5000)
	for index := range hashes {
		object := maintenanceObject(test, map[string]any{"filler": index})
		hashes[index], bodies[index], children[index] = string(object.Hash), object.Bytes, string(domain.HashContent([]byte(fmt.Sprint(index))))
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes)
 SELECT $1,hash,bytes FROM unnest($2::text[],$3::bytea[]) filler(hash,bytes)`, source, hashes, bodies); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_display_object_edges_v1(source_hash,parent_hash,child_hash)
 SELECT $1,parent_hash,child_hash FROM unnest($2::text[],$3::text[]) filler(parent_hash,child_hash)`, source, hashes, children); err != nil {
		test.Fatal(err)
	}
	objects := make([]domain.DocRangeObject, domain.MaxDocEventDisplayStepObjects)
	for index := range objects {
		objects[index] = maintenanceObject(test, map[string]any{"selected": index, "child": domain.HashContent([]byte(fmt.Sprint(index)))})
	}
	trace := &docDisplayWriteCostTrace{}
	config := storage.pool.Config().ConnConfig.Copy()
	config.Tracer = trace
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close(ctx)
	transaction, err := connection.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if err := putDocDisplayObjects(ctx, transaction, source, objects); err != nil {
		test.Fatal(err)
	}
	if trace.proof == "" {
		test.Fatal("actual batch proof was not traced")
	}
	proof, arguments := trace.proof, trace.arguments
	insert, insertArguments := trace.insert, trace.insertArguments
	if _, err := transaction.Exec(ctx, `ANALYZE doc_display_objects_v1; ANALYZE doc_display_object_edges_v1`); err != nil {
		test.Fatal(err)
	}
	for _, sequential := range []string{"on", "off"} {
		if _, err := transaction.Exec(ctx, `SET LOCAL enable_seqscan=`+sequential); err != nil {
			test.Fatal(err)
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			for _, selected := range []struct {
				operation, query string
				arguments        []any
			}{{"proof", proof, arguments}, {"known_insert", insert, insertArguments}} {
				encoded := explainDocToolOrdersPGTraceQuery(test, ctx, transaction, selected.query, selected.arguments, mode)
				var plans []struct{ Plan map[string]any }
				if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
					test.Fatal("invalid proof plan", err)
				}
				maximumRows, maximumFiltered, maximumLoops := float64(0), float64(0), float64(0)
				var inspect func(map[string]any)
				inspect = func(plan map[string]any) {
					if relation, ok := plan["Relation Name"].(string); ok && (relation == "doc_display_objects_v1" || relation == "doc_display_object_edges_v1") {
						maximumRows = max(maximumRows, plan["Actual Rows"].(float64))
						maximumLoops = max(maximumLoops, plan["Actual Loops"].(float64))
						if filtered, ok := plan["Rows Removed by Filter"].(float64); ok {
							maximumFiltered = max(maximumFiltered, filtered)
						}
					}
					if children, ok := plan["Plans"].([]any); ok {
						for _, child := range children {
							inspect(child.(map[string]any))
						}
					}
				}
				inspect(plans[0].Plan)
				test.Logf("operation=%s mode=%s seqscan=%s unrelated_objects=5000 selected_parents=%d max_relation_rows=%g max_relation_filtered=%g max_relation_loops=%g", selected.operation, mode, sequential, len(objects), maximumRows, maximumFiltered, maximumLoops)
				if maximumRows > float64(len(objects)) || maximumFiltered != 0 || maximumLoops > float64(2*len(objects)) {
					test.Fatalf("bounded proof scanned unrelated source history: %s", encoded)
				}
			}
		}
	}
}

func (*docDisplayWriteCostTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDocDisplayObjectWritePGQueryCost(test *testing.T) {
	for _, count := range []int{32, 256} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			source, _ := maintenanceSource(test, ctx, storage, "bounded batch query cost")
			objects := make([]domain.DocRangeObject, count)
			for index := range objects {
				objects[index] = maintenanceObject(test, map[string]any{"ordinal": index, "child": domain.HashContent([]byte(fmt.Sprint(index)))})
			}
			trace := &docDisplayWriteCostTrace{}
			config := storage.pool.Config().ConnConfig.Copy()
			config.Tracer = trace
			connection, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				test.Fatal(err)
			}
			defer connection.Close(ctx)
			for _, replay := range []bool{false, true} {
				var beforeRevision int64
				if err := storage.pool.QueryRow(ctx, `SELECT reference_revision FROM doc_display_sources_v1 WHERE source_hash=$1`, source).Scan(&beforeRevision); err != nil {
					test.Fatal(err)
				}
				transaction, err := connection.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				trace.queries = 0
				err = putDocDisplayObjects(ctx, transaction, source, objects)
				queries := trace.queries
				if err != nil {
					rollbackPG(transaction)
					test.Fatal(err)
				}
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				var afterRevision int64
				if err := storage.pool.QueryRow(ctx, `SELECT reference_revision FROM doc_display_sources_v1 WHERE source_hash=$1`, source).Scan(&afterRevision); err != nil {
					test.Fatal(err)
				}
				perBatch := 5
				if replay {
					perBatch = 3
				}
				limit := 1 + perBatch*((count+domain.MaxDocEventDisplayStepObjects-1)/domain.MaxDocEventDisplayStepObjects)
				test.Logf("objects=%d replay=%t client_sql_queries=%d target_max=%d source_revision_delta=%d excludes_begin_commit_triggers_and_network_latency=true", count, replay, queries, limit, afterRevision-beforeRevision)
				if queries > limit {
					test.Errorf("object writes still scale SQL exchanges per object: got %d, want <=%d", queries, limit)
				}
				if replay && afterRevision-beforeRevision != 1 {
					test.Error("covered replay fired object insertion fences instead of one explicit source fence")
				}
			}
		})
	}
}
