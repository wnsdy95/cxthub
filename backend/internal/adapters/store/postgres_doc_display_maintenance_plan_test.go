//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayMaintenancePlanTracePG struct {
	queries   []string
	arguments [][]any
}

func (trace *docDisplayMaintenancePlanTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(query.SQL, "SELECT locked_cursor.after_source,work.source_hash,work.ready") || strings.HasPrefix(query.SQL, "SELECT generation_hash,substring(bytes") {
		trace.queries = append(trace.queries, query.SQL)
		trace.arguments = append(trace.arguments, slices.Clone(query.Args))
	}
	return ctx
}

func (*docDisplayMaintenancePlanTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

type docDisplayMaintenancePlanSourcePG struct {
	hash, event domain.ContentHash
	raw         []byte
	generation  domain.DocRangeObject
}

func seedDocDisplayMaintenancePlanSourcesPG(test *testing.T, ctx context.Context, storage *PostgresStore, sources []docDisplayMaintenancePlanSourcePG, missing, ready domain.ContentHash) {
	test.Helper()
	hashes, events, phases := make([]string, len(sources)), make([]string, len(sources)), make([]string, len(sources))
	bodies := make([][]byte, len(sources))
	var generationSources, generationEvents, generationHashes []string
	var generationBodies [][]byte
	for index, source := range sources {
		hashes[index], events[index], bodies[index] = string(source.hash), string(source.event), source.raw
		phases[index] = "index"
		if source.hash == missing || index%2 != 0 && source.hash != ready {
			phases[index] = "idle"
		}
		if source.hash != missing {
			generationSources = append(generationSources, string(source.hash))
			generationEvents = append(generationEvents, string(source.event))
			generationHashes = append(generationHashes, string(source.generation.Hash))
			generationBodies = append(generationBodies, source.generation.Bytes)
		}
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if tag, err := transaction.Exec(ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 SELECT event_hash,$4,source_hash,bytes FROM unnest($1::text[],$2::text[],$3::bytea[]) source(event_hash,source_hash,bytes)`, events, hashes, bodies, domain.DocDisplayProjectionProfile); err != nil || tag.RowsAffected() != int64(len(sources)) {
		test.Fatal("seed typed maintenance plan sources", tag.RowsAffected(), err)
	}
	if tag, err := transaction.Exec(ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 SELECT event_hash,$5,source_hash,generation_hash,bytes FROM unnest($1::text[],$2::text[],$3::text[],$4::bytea[])
 generation(event_hash,source_hash,generation_hash,bytes)`, generationEvents, generationSources, generationHashes, generationBodies, domain.DocDisplayProjectionProfile); err != nil || tag.RowsAffected() != int64(len(generationSources)) {
		test.Fatal("seed hash-consistent synthetic generation lookup metadata", tag.RowsAffected(), err)
	}
	if tag, err := transaction.Exec(ctx, `INSERT INTO doc_display_maintenance_v1(source_hash,phase,due_at)
 SELECT source_hash,phase,CASE WHEN source_hash=$3 THEN '-infinity'::timestamptz ELSE 'infinity'::timestamptz END
 FROM unnest($1::text[],$2::text[]) work(source_hash,phase)
 ON CONFLICT(source_hash) DO UPDATE SET phase=EXCLUDED.phase,due_at=EXCLUDED.due_at`, hashes, phases, ready); err != nil || tag.RowsAffected() != int64(len(sources)) {
		test.Fatal("seed future, idle and ready ring metadata", tag.RowsAffected(), err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func assertDocDisplayMaintenancePointPlanPG(test *testing.T, encoded []byte, relation, index, condition string, expectedRows int) {
	test.Helper()
	var plans []struct {
		Plan struct {
			docEventDisplayReusePlan
			SharedHits  float64 `json:"Shared Hit Blocks"`
			SharedReads float64 `json:"Shared Read Blocks"`
			LocalHits   float64 `json:"Local Hit Blocks"`
			LocalReads  float64 `json:"Local Read Blocks"`
			TempReads   float64 `json:"Temp Read Blocks"`
			TempWrites  float64 `json:"Temp Written Blocks"`
		}
	}
	if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
		test.Fatal("invalid maintenance probe plan", err, string(encoded))
	}
	plan := plans[0].Plan
	blocks := plan.SharedHits + plan.SharedReads + plan.LocalHits + plan.LocalReads
	if blocks <= 0 || blocks > 64 || plan.TempReads != 0 || plan.TempWrites != 0 {
		test.Fatalf("maintenance point probe exceeded its fixed 64-block read budget or spilled: %s", encoded)
	}
	probes, limits := 0, 0
	var inspect func(docEventDisplayReusePlan)
	inspect = func(node docEventDisplayReusePlan) {
		if node.Rows != float64(expectedRows) || node.Loops != 1 || node.Filter != "" || node.Filtered != 0 || node.JoinFiltered != 0 || node.IndexRechecked != 0 {
			test.Fatalf("maintenance probe visited or filtered unrelated history: %s", encoded)
		}
		if node.NodeType == "Limit" && relation == "doc_display_maintenance_v1" {
			limits++
			if node.Relation != "" || node.Index != "" || len(node.Plans) != 1 {
				test.Fatalf("ring probe lost its single bounded index child: %s", encoded)
			}
		} else {
			probes++
			if node.NodeType != "Index Scan" && node.NodeType != "Index Only Scan" || node.Relation != relation || node.Index != index || !strings.Contains(node.Condition, condition) || len(node.Plans) != 0 {
				test.Fatalf("maintenance probe is not an exact source index seek: %s", encoded)
			}
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plan.docEventDisplayReusePlan)
	expectedLimits := 0
	if relation == "doc_display_maintenance_v1" {
		expectedLimits = 1
	}
	if probes != 1 || limits != expectedLimits {
		test.Fatalf("maintenance probe changed its bounded access shape: %s", encoded)
	}
	test.Logf("relation=%s index=%s actual_rows=%d loops=1 filtered=0 read_blocks=%g physical_reads=%g", relation, index, expectedRows, blocks, plan.SharedReads+plan.LocalReads)
}

type docDisplayMaintenanceCursorPlanPG struct {
	docEventDisplayReusePlan
	Children    []json.RawMessage `json:"Plans"`
	JoinType    string            `json:"Join Type"`
	JoinFilter  string            `json:"Join Filter"`
	Alias       string            `json:"Alias"`
	SharedHits  float64           `json:"Shared Hit Blocks"`
	SharedReads float64           `json:"Shared Read Blocks"`
	LocalHits   float64           `json:"Local Hit Blocks"`
	LocalReads  float64           `json:"Local Read Blocks"`
	TempReads   float64           `json:"Temp Read Blocks"`
	TempWrites  float64           `json:"Temp Written Blocks"`
}

func assertDocDisplayMaintenanceLockedCursorPlanPG(test *testing.T, encoded []byte, index, condition string, expectedRows int) {
	test.Helper()
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
		test.Fatal("invalid locked-cursor maintenance plan", err, string(encoded))
	}
	decode := func(raw json.RawMessage) docDisplayMaintenanceCursorPlanPG {
		var node docDisplayMaintenanceCursorPlanPG
		if err := json.Unmarshal(raw, &node); err != nil {
			test.Fatal("invalid locked-cursor plan node", err, string(encoded))
		}
		return node
	}
	root := decode(plans[0].Plan)
	blocks := root.SharedHits + root.SharedReads + root.LocalHits + root.LocalReads
	if blocks <= 0 || blocks > 64 || root.TempReads != 0 || root.TempWrites != 0 {
		test.Fatalf("locked-cursor probe exceeded its total fixed 64-block read budget or spilled: %s", encoded)
	}
	checkOuter := func(node docDisplayMaintenanceCursorPlanPG, kind, alias string, children int) {
		if node.NodeType != kind || node.Alias != alias || node.Rows != 1 || node.Loops != 1 || len(node.Children) != children ||
			node.Relation != "" || node.Index != "" || node.Condition != "" || node.Filter != "" || node.JoinFilter != "" ||
			kind != "Nested Loop" && node.JoinType != "" ||
			node.Filtered != 0 || node.JoinFiltered != 0 || node.IndexRechecked != 0 || node.TempReads != 0 || node.TempWrites != 0 {
			test.Fatalf("locked-cursor probe changed its single-row outer access shape: %s", encoded)
		}
	}
	checkOuter(root, "Nested Loop", "", 2)
	if root.JoinType != "Left" {
		test.Fatalf("locked-cursor probe lost its one-row left join on a source miss: %s", encoded)
	}
	subquery := decode(root.Children[0])
	checkOuter(subquery, "Subquery Scan", "locked_cursor", 1)
	lock := decode(subquery.Children[0])
	checkOuter(lock, "LockRows", "", 1)
	cursor := decode(lock.Children[0])
	if cursor.Relation != "doc_display_maintenance_cursor_v1" || cursor.Alias != "doc_display_maintenance_cursor_v1" ||
		cursor.Rows != 1 || cursor.Loops != 1 || len(cursor.Children) != 0 || cursor.JoinType != "" || cursor.JoinFilter != "" ||
		cursor.Filtered != 0 || cursor.JoinFiltered != 0 || cursor.IndexRechecked != 0 || cursor.TempReads != 0 || cursor.TempWrites != 0 ||
		cursor.Filter != "" && cursor.Filter != "singleton" || cursor.Condition != "" && cursor.Condition != "(singleton = true)" {
		test.Fatalf("locked-cursor probe did not read exactly the singleton cursor row: %s", encoded)
	}
	if cursor.NodeType == "Seq Scan" {
		if cursor.Index != "" || cursor.Condition != "" {
			test.Fatalf("singleton cursor sequential scan acquired an unexpected index condition: %s", encoded)
		}
	} else if (cursor.NodeType != "Index Scan" && cursor.NodeType != "Index Only Scan") || cursor.Index != "doc_display_maintenance_cursor_v1_pkey" {
		test.Fatalf("only the singleton cursor may use a non-Source scan: %s", encoded)
	}
	var source docDisplayMaintenanceCursorPlanPG
	if err := json.Unmarshal(root.Children[1], &source); err != nil || source.NodeType != "Limit" {
		test.Fatalf("locked-cursor probe lost its direct bounded Source subtree: %s", encoded)
	}
	sourcePlan, err := json.Marshal([]struct {
		Plan json.RawMessage `json:"Plan"`
	}{{Plan: root.Children[1]}})
	if err != nil {
		test.Fatal(err)
	}
	assertDocDisplayMaintenancePointPlanPG(test, sourcePlan, "doc_display_maintenance_v1", index, condition, expectedRows)
	test.Logf("cursor_rows=1 loops=1 source_rows=%d total_read_blocks=%g physical_reads=%g", expectedRows, blocks, root.SharedReads+root.LocalReads)
}

func TestDocDisplayMaintenancePGBoundedRingAndGenerationPlans(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	var database string
	if err := storage.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		test.Fatal(err)
	}
	if _, owned := docDisplayOwnedFixturesPG.Load(database); !owned || database != storage.pool.Config().ConnConfig.Database {
		test.Fatal("maintenance plan fixture must use its actual owned database", database)
	}
	trace := &docDisplayMaintenancePlanTracePG{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	config.MinConns, config.MaxConns = 0, 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	observed := &PostgresStore{pool: pool}
	if _, err := observed.selectDocDisplayMaintenance(ctx); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("empty owned fixture must trace both production ring probes", err)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if err := observed.rootDocDisplayGeneration(ctx, transaction, &docDisplayMaintenanceState{source: domain.HashContent([]byte("maintenance plan trace miss"))}); err != nil {
		test.Fatal("trace production generation lookup", err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if len(trace.queries) != 3 || len(trace.arguments) != 3 || len(trace.arguments[0]) != 1 || len(trace.arguments[1]) != 1 || trace.arguments[0][0] != 1 || trace.arguments[1][0] != 1 || len(trace.arguments[2]) != 2 || trace.arguments[2][1] != domain.MaxDocDisplaySourceBytes+1 {
		test.Fatal("production maintenance queries or body bound were not traced", trace.queries, trace.arguments)
	}
	const maximumHistory = 640
	sources := make([]docDisplayMaintenancePlanSourcePG, maximumHistory+3)
	for ordinal := range sources {
		event := domain.HashContent([]byte(fmt.Sprintf("maintenance point-plan event %d", ordinal)))
		source := domain.DocEventDisplaySource{Version: 1, Profile: domain.DocDisplayProjectionProfile, EventHash: event, EventBytes: 100, Kind: domain.EventReasoning}
		raw, err := source.Encode()
		if err != nil {
			test.Fatal(err)
		}
		sources[ordinal] = docDisplayMaintenancePlanSourcePG{hash: domain.HashContent(raw), event: event, raw: raw,
			generation: maintenanceObject(test, map[string]any{"padding": strings.Repeat("p", 1280), "plan": domain.HashContent([]byte(fmt.Sprintf("maintenance plan root %d", ordinal))), "synthetic": "query-plan metadata only"})}
		if len(sources[ordinal].generation.Bytes) > domain.MaxDocDisplaySourceBytes {
			test.Fatal("synthetic generation exceeded the existing body budget")
		}
	}
	slices.SortFunc(sources, func(first, second docDisplayMaintenancePlanSourcePG) int {
		return strings.Compare(string(first.hash), string(second.hash))
	})
	missing, future, ready := sources[0], sources[1], sources[len(sources)-1]
	history := sources[2 : len(sources)-1]
	test.Log("Typed source headers and canonical hash-consistent synthetic generations exercise SQL plans only; synthetic generation bytes are not typed publication evidence. Bounded inline generation payloads make the 64-row lookup nontrivial without changing planner costs. Read/row limits are identical at 64 and 640 unrelated sources, work rows and generations; the existing fixture context deadline is unchanged.")
	seedDocDisplayMaintenancePlanSourcesPG(test, ctx, storage, []docDisplayMaintenancePlanSourcePG{missing, ready, future}, missing.hash, ready.hash)
	previous := 0
	for _, count := range []int{64, maximumHistory} {
		seedDocDisplayMaintenancePlanSourcesPG(test, ctx, storage, history[previous:count], missing.hash, ready.hash)
		previous = count
		var sourceCount, workCount, generationCount, idleCount, futureCount, readyCount int
		if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM doc_display_sources_v1),
 (SELECT count(*) FROM doc_display_generations_v1),count(*),count(*) FILTER (WHERE phase='idle'),
 count(*) FILTER (WHERE phase<>'idle' AND due_at>clock_timestamp()),count(*) FILTER (WHERE phase<>'idle' AND due_at<=clock_timestamp())
 FROM doc_display_maintenance_v1`).Scan(&sourceCount, &generationCount, &workCount, &idleCount, &futureCount, &readyCount); err != nil || sourceCount != count+3 || workCount != count+3 || generationCount != count+2 || idleCount != count/2+1 || futureCount != count/2+1 || readyCount != 1 {
			test.Fatal("maintenance plan fixture lost unrelated metadata, idle rows, future prefix or ready tail", count, sourceCount, workCount, generationCount, idleCount, futureCount, readyCount, err)
		}
		test.Run(fmt.Sprintf("unrelated=%d", count), func(test *testing.T) {
			transaction, err := pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			if _, err := transaction.Exec(ctx, `ANALYZE doc_display_sources_v1; ANALYZE doc_display_maintenance_v1; ANALYZE doc_display_generations_v1`); err != nil {
				test.Fatal(err)
			}
			for _, sequential := range []string{"on", "off"} {
				if _, err := transaction.Exec(ctx, `SET LOCAL enable_seqscan=`+sequential); err != nil {
					test.Fatal(err)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for _, selected := range []struct {
						name, relation, index, condition string
						query                            int
						cursor, expected                 domain.ContentHash
						ready                            bool
					}{
						{"forward_future", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash >", 0, missing.hash, future.hash, false},
						{"forward_ready", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash >", 0, history[count-1].hash, ready.hash, true},
						{"forward_miss", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash >", 0, ready.hash, "", false},
						{"wrap_future", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash <=", 1, ready.hash, future.hash, false},
						{"wrap_inclusive", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash <=", 1, future.hash, future.hash, false},
						{"wrap_miss", "doc_display_maintenance_v1", "doc_display_maintenance_v1_ring", "source_hash <=", 1, missing.hash, "", false},
						{"generation_hit", "doc_display_generations_v1", "doc_display_generations_v1_source", "source_hash =", 2, future.hash, future.generation.Hash, false},
						{"generation_miss", "doc_display_generations_v1", "doc_display_generations_v1_source", "source_hash =", 2, missing.hash, "", false},
					} {
						test.Run(selected.name+"/"+mode+"/seqscan="+sequential, func(test *testing.T) {
							arguments := slices.Clone(trace.arguments[selected.query])
							query := trace.queries[selected.query]
							var actual domain.ContentHash
							var ready bool
							var raw []byte
							expectedRows := 1
							if selected.expected == "" {
								expectedRows = 0
							}
							if selected.query == 2 {
								arguments[0] = selected.cursor
								err = transaction.QueryRow(ctx, query, arguments...).Scan(&actual, &raw)
								if expectedRows == 0 {
									if !errors.Is(err, pgx.ErrNoRows) {
										test.Fatal("maintenance generation miss returned data", actual, err)
									}
								} else if err != nil || actual != selected.expected || !bytes.Equal(raw, future.generation.Bytes) || domain.HashContent(raw) != actual {
									test.Fatal("maintenance lookup changed the exact generation proof", actual, err)
								}
							} else {
								if tag, err := transaction.Exec(ctx, `UPDATE doc_display_maintenance_cursor_v1 SET after_source=$1 WHERE singleton`, selected.cursor); err != nil || tag.RowsAffected() != 1 {
									test.Fatal("set the actual singleton cursor for the ring probe", tag, err)
								}
								var observedAfter string
								var nullableSource *string
								var nullableReady *bool
								if err := transaction.QueryRow(ctx, query, arguments...).Scan(&observedAfter, &nullableSource, &nullableReady); err != nil || observedAfter != string(selected.cursor) {
									test.Fatal("ring probe did not return its locked actual cursor row", observedAfter, err)
								}
								if expectedRows == 0 {
									if nullableSource != nil || nullableReady != nil {
										test.Fatal("ring miss returned non-null candidate fields", nullableSource, nullableReady)
									}
								} else {
									if nullableSource == nil || nullableReady == nil {
										test.Fatal("ring hit omitted its candidate or eligibility", nullableSource, nullableReady)
									}
									actual, ready = domain.ContentHash(*nullableSource), *nullableReady
									if actual != selected.expected || ready != selected.ready {
										test.Fatal("ring probe changed the exact candidate or eligibility", actual, ready)
									}
								}
							}
							encoded := explainDocToolOrdersPGTraceQuery(test, ctx, transaction, query, arguments, mode)
							if selected.query == 2 {
								assertDocDisplayMaintenancePointPlanPG(test, encoded, selected.relation, selected.index, selected.condition, expectedRows)
							} else {
								assertDocDisplayMaintenanceLockedCursorPlanPG(test, encoded, selected.index, selected.condition, expectedRows)
							}
						})
					}
				}
			}
		})
	}
}
