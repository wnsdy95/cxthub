//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingProjectionReadCostQuery struct {
	statement string
	budget    *repositoryViewSourceBudgetQuery
}

type pendingProjectionReadCostTx struct {
	pgx.Tx
	queries    []pendingProjectionReadCostQuery
	statements []string
}

func (observed *pendingProjectionReadCostTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	observed.statements = append(observed.statements, statement)
	rows, err := observed.Tx.Query(ctx, statement, args...)
	if err != nil || !strings.Contains(statement, "pending_projection_parts") {
		return rows, err
	}
	budget := &repositoryViewSourceBudgetQuery{args: append([]any(nil), args...), columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			budget.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, pendingProjectionReadCostQuery{statement: statement, budget: budget})
	return &repositoryViewSourceBudgetRows{Rows: rows, record: budget}, nil
}

type pendingProjectionReadCostRow struct {
	rows pgx.Rows
	err  error
}

func (observed pendingProjectionReadCostRow) Scan(destinations ...any) error {
	if observed.err != nil {
		return observed.err
	}
	defer observed.rows.Close()
	if !observed.rows.Next() {
		if err := observed.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return observed.rows.Scan(destinations...)
}

func (observed *pendingProjectionReadCostTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	rows, err := observed.Query(ctx, statement, args...)
	return pendingProjectionReadCostRow{rows: rows, err: err}
}

type pendingProjectionReadPlanPG struct {
	NodeType    string                        `json:"Node Type"`
	Relation    string                        `json:"Relation Name"`
	Index       string                        `json:"Index Name"`
	Condition   string                        `json:"Index Cond"`
	Rows        float64                       `json:"Actual Rows"`
	Loops       float64                       `json:"Actual Loops"`
	Removed     float64                       `json:"Rows Removed by Filter"`
	Rechecked   float64                       `json:"Rows Removed by Index Recheck"`
	SharedHits  float64                       `json:"Shared Hit Blocks"`
	SharedReads float64                       `json:"Shared Read Blocks"`
	Plans       []pendingProjectionReadPlanPG `json:"Plans"`
}

func pendingProjectionAssertPointPlanPG(t *testing.T, ctx context.Context, transaction pgx.Tx, query pendingProjectionReadCostQuery, expected int) {
	t.Helper()
	var raw []byte
	if err := transaction.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+query.statement, query.budget.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan pendingProjectionReadPlanPG }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("invalid ready read plan: %s err=%v", raw, err)
	}
	seeks := 0
	var inspect func(pendingProjectionReadPlanPG)
	inspect = func(plan pendingProjectionReadPlanPG) {
		if plan.NodeType == "Sort" || plan.NodeType == "Unique" {
			t.Fatalf("ready read sorted or deduplicated history: %s", raw)
		}
		if plan.Relation == "pending_projection_parts" {
			seeks++
			if plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" {
				t.Fatalf("ready read did not use point index lookups: %s", raw)
			}
			if plan.Index != "pending_projection_parts_latest" && plan.Index != "pending_projection_parts_pkey" {
				t.Fatalf("unexpected ready part index: %s", raw)
			}
			if strings.Contains(query.statement, "ORDER BY source_seq DESC LIMIT 1") && plan.Index != "pending_projection_parts_latest" {
				t.Fatalf("latest lookup did not seek by kind/key before sequence: %s", raw)
			}
			for _, column := range []string{"repo_id", "dataset", "source_seq", "kind", "entity_key"} {
				if !strings.Contains(plan.Condition, column) {
					t.Fatalf("ready part index lacks %s identity bound: %s", column, raw)
				}
			}
			if plan.Rows != 1 || plan.Loops != float64(expected) || plan.Removed != 0 || plan.Rechecked != 0 {
				t.Fatalf("ready read visited rows outside the current keys: %s", raw)
			}
			if plan.SharedHits+plan.SharedReads > 64*float64(expected+1) {
				t.Fatalf("ready key lookup touched excessive historical buffers: %s", raw)
			}
		}
		for _, child := range plan.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
	if seeks != 1 {
		t.Fatalf("want one parameterized part index node, found %d: %s", seeks, raw)
	}
}

func pendingProjectionObservedReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, explain bool) (domain.PendingProjectionRead, []pendingProjectionReadCostQuery) {
	t.Helper()
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(transaction)
	observed := &pendingProjectionReadCostTx{Tx: transaction}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: observed, owner: storage, readOnly: true})
	read, err := storage.ReadPendingProjection(bound, repo)
	if err != nil || !read.Enabled || !read.Ready || read.NeedsAttention {
		t.Fatalf("observed ready read: enabled=%v ready=%v needs_attention=%v err=%v", read.Enabled, read.Ready, read.NeedsAttention, err)
	}
	for _, statement := range observed.statements {
		for _, forbidden := range []string{"repository_view_source_images", "pending_projection_inputs", "FROM snapshots", "FROM pending_contexts", "DISTINCT ON"} {
			if strings.Contains(statement, forbidden) {
				t.Fatalf("warm read traversed source or historical inventory: %s", statement)
			}
		}
	}
	if explain {
		for _, query := range observed.queries {
			expected := 1
			if len(query.budget.args) == 5 {
				expected = len(read.Parts) - 1
			}
			pendingProjectionAssertPointPlanPG(t, bound, transaction, query, expected)
		}
	}
	return read, observed.queries
}

func pendingProjectionAssertReadTransferPG(t *testing.T, read domain.PendingProjectionRead, queries []pendingProjectionReadCostQuery) int {
	t.Helper()
	if len(queries) != 4 || len(read.Parts) < 2 {
		t.Fatalf("want header and body descriptor/payload queries, got %d queries and %d parts", len(queries), len(read.Parts))
	}
	wanted := map[[2]string]bool{}
	imageBytes := 0
	for _, part := range read.Parts {
		imageBytes += len(part.Value)
		if part.Kind != "header" || part.Key != "view" {
			wanted[[2]string{part.Kind, part.Key}] = true
		}
	}
	images, transferredImages, transferredValues := 0, 0, 0
	for position, query := range queries {
		budget := query.budget
		expectedRows, expectedImages := 1, 0
		if position == 1 {
			expectedImages = 1
		}
		if position == 3 {
			expectedRows, expectedImages = len(wanted), len(wanted)
		}
		if budget.rows != expectedRows || budget.images != expectedImages || (position%2 == 0 && (budget.jsonColumns != 0 || budget.imageBytes != 0)) {
			t.Fatalf("query %d transferred unselected rows or payloads: %+v", position, budget)
		}
		if position >= 2 {
			if len(budget.args) != 5 {
				t.Fatalf("body query lacks exact key parameters: %+v", budget.args)
			}
			kinds, kindsOK := budget.args[3].([]string)
			keys, keysOK := budget.args[4].([]string)
			if !kindsOK || !keysOK || len(kinds) != len(wanted) || len(keys) != len(wanted) {
				t.Fatalf("body query selected historical key inventory: %+v", budget.args)
			}
			seen := map[[2]string]bool{}
			for index, kind := range kinds {
				identity := [2]string{kind, keys[index]}
				if !wanted[identity] || seen[identity] {
					t.Fatalf("body query requested an unlisted or duplicate identity: %v", identity)
				}
				seen[identity] = true
			}
		}
		images += budget.images
		transferredImages += budget.imageBytes
		transferredValues += budget.valueBytes
	}
	if images != len(read.Parts) || transferredImages != imageBytes || transferredValues > domain.MaxPendingProjectionOutputBytes {
		t.Fatalf("warm read transferred discarded history: images=%d wanted=%d image_bytes=%d wanted_bytes=%d values=%d", images, len(read.Parts), transferredImages, imageBytes, transferredValues)
	}
	return transferredValues
}

func TestPGPendingProjectionReadCostCurrentKeysOnly(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	before, beforeQueries := pendingProjectionObservedReadPG(t, ctx, storage, repo, false)
	beforeBytes := pendingProjectionAssertReadTransferPG(t, before, beforeQueries)
	source := catalogReadStatePG(t, ctx, storage, repo)
	if job.InputSequence <= 0 {
		t.Fatal("cost fixture needs an older source sequence")
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_projection_parts(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		SELECT $1,$2,0,'pending','cost-retired-'||lpad(position::text,5,'0'),false,
		jsonb_build_object('not_a_projection',repeat('x',2500)) FROM generate_series(1,4096) position`, string(repo), job.Dataset)
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_projection_parts(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		SELECT repo_id,dataset,$3,kind,entity_key,true,NULL FROM pending_projection_parts
		WHERE repo_id=$1 AND dataset=$2 AND source_seq=0 AND entity_key LIKE 'cost-retired-%'`, string(repo), job.Dataset, job.InputSequence)
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_projection_parts(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		SELECT repo_id,dataset,0,kind,entity_key,false,'{"not_a_projection":"old current-key payload"}'::jsonb
		FROM pending_projection_parts WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3 AND NOT deleted`, string(repo), job.Dataset, job.InputSequence)
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_projection_parts(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		SELECT repo_id,dataset,$3+1,kind,entity_key,false,'{"not_a_projection":"future current-key payload"}'::jsonb
		FROM pending_projection_parts WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3 AND NOT deleted`, string(repo), job.Dataset, job.InputSequence)
	var historicalBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT sum(octet_length(image::text)) FROM pending_projection_parts
		WHERE repo_id=$1 AND dataset=$2 AND source_seq=0`, string(repo), job.Dataset).Scan(&historicalBytes); err != nil || historicalBytes < 9<<20 || historicalBytes > 12<<20 {
		t.Fatalf("history fixture outside its byte bound: bytes=%d err=%v", historicalBytes, err)
	}
	catalogExecPG(t, ctx, storage.pool, `ANALYZE pending_projection_parts`)
	after, afterQueries := pendingProjectionObservedReadPG(t, ctx, storage, repo, true)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("historical, unlisted, or future parts changed the ready read")
	}
	decoded, err := domain.DecodePendingProjection(ctx, repo, after.Parts)
	if err != nil || !reflect.DeepEqual(decoded, view) {
		t.Fatalf("poison history reached projection decoding: err=%v", err)
	}
	if afterBytes := pendingProjectionAssertReadTransferPG(t, after, afterQueries); afterBytes != beforeBytes {
		t.Fatalf("history increased result-value transfer: before=%d after=%d", beforeBytes, afterBytes)
	}
	if current := catalogReadStatePG(t, ctx, storage, repo); current != source {
		t.Fatalf("derived history changed original source state: before=%+v after=%+v", source, current)
	}
}

func TestPGPendingProjectionNeedsAttentionSurvivesExpiredRetention(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	readyJob := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	readyView, readyParts := pendingProjectionPartsPG(t, ctx, storage, readyJob)
	if err := storage.PublishPendingProjection(ctx, readyJob, readyParts); err != nil {
		t.Fatal(err)
	}
	ready := pendingProjectionReadPG(t, ctx, storage, readyJob, readyView)
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"after terminal failure"') WHERE repo_id=$1`, string(repo))
	failed := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	if err := storage.FailPendingProjection(ctx, failed, "resource_limit", false); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET retention_until=clock_timestamp()-interval '1 day',
		next_attempt=clock_timestamp()-interval '1 day' WHERE repo_id=$1`, string(repo))
	source := catalogReadStatePG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), source.head)
	var before, after []byte
	if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(work) FROM pending_projection_work work WHERE repo_id=$1`, string(repo)).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("terminal work requeued after retention expiry: %v", err)
		}
	}
	if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(work) FROM pending_projection_work work WHERE repo_id=$1`, string(repo)).Scan(&after); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("unsuccessful claims changed terminal state: err=%v", err)
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || !read.Enabled || !read.NeedsAttention || read.Ready || len(read.Parts) != 0 || read.Applied != ready.Applied || read.Source.Sequence != source.head {
		t.Fatalf("terminal failure receipt: %+v err=%v", read, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		parts, err := storage.readPendingProjectionReadyParts(bound, repo, ready.Dataset, ready.Applied.Sequence)
		if err != nil || !reflect.DeepEqual(parts, ready.Parts) {
			t.Fatalf("failure changed immutable previous ready parts: err=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	read, err = storage.ReadPendingProjection(ctx, repo)
	if err != nil || read.NeedsAttention || read.Ready || read.Applied != ready.Applied {
		t.Fatalf("explicit configuration did not clear terminal status while preserving ready: %+v err=%v", read, err)
	}
	restarted := pendingProjectionClaimPG(t, ctx, storage, repo)
	if restarted.Dataset == failed.Dataset || restarted.Fence <= failed.Fence || restarted.Phase != "capture" || restarted.Cursor != "" || restarted.AppliedSequence != -1 {
		t.Fatalf("explicit configuration did not bootstrap terminal work: %+v", restarted)
	}
	restarted = pendingProjectionCapturePG(t, ctx, storage, restarted, 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, restarted)
	if err := storage.PublishPendingProjection(ctx, restarted, parts); err != nil {
		t.Fatal(err)
	}
	if read := pendingProjectionReadPG(t, ctx, storage, restarted, view); read.NeedsAttention {
		t.Fatal("replacement publication remained terminal")
	}
}

func TestPGPendingProjectionLostPartialCursorRebootstraps(t *testing.T) {
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, repo, _ := pendingProjectionPG(t)
			if mode == "delta" {
				ready := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
				_, parts := pendingProjectionPartsPG(t, ctx, storage, ready)
				if err := storage.PublishPendingProjection(ctx, ready, parts); err != nil {
					t.Fatal(err)
				}
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"lost cursor"') WHERE repo_id=$1`, string(repo))
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='after lost cursor' WHERE repo_id=$1`, string(repo))
			}
			source := catalogReadStatePG(t, ctx, storage, repo)
			job := pendingProjectionClaimPG(t, ctx, storage, repo)
			request := job.SourceRequest()
			request.Limit = 1
			page := repositoryViewSourcePagePG(t, ctx, storage, repo, request)
			if page.NextCursor == "" || page.Checkpoint != nil || page.Mode != mode {
				t.Fatalf("fixture did not create partial %s capture: %+v", mode, page)
			}
			if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET cursor='' WHERE repo_id=$1`, string(repo))
			restarted := pendingProjectionClaimPG(t, ctx, storage, repo)
			if restarted.Dataset == job.Dataset || restarted.Epoch != job.Epoch || restarted.Fence <= job.Fence || restarted.Phase != "capture" ||
				restarted.Cursor != "" || restarted.AppliedSequence != -1 || restarted.InputSequence != -1 {
				t.Fatalf("lost cursor reused partial input dataset: old=%+v new=%+v", job, restarted)
			}
			var obsolete int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM pending_projection_inputs WHERE repo_id=$1 AND dataset=$2`, string(repo), job.Dataset).Scan(&obsolete); err != nil || obsolete != 0 {
				t.Fatalf("lost cursor retained abandoned inputs: rows=%d err=%v", obsolete, err)
			}
			restarted = pendingProjectionCapturePG(t, ctx, storage, restarted, 1)
			view, parts := pendingProjectionPartsPG(t, ctx, storage, restarted)
			if err := storage.PublishPendingProjection(ctx, restarted, parts); err != nil {
				t.Fatal(err)
			}
			pendingProjectionReadPG(t, ctx, storage, restarted, view)
			if current := catalogReadStatePG(t, ctx, storage, repo); current != source {
				t.Fatalf("derived recovery advanced source state: before=%+v after=%+v", source, current)
			}
		})
	}
}

func TestPGPendingProjectionDerivedRetentionBoundedAndSnapshotSafe(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	counts := pendingProjectionCountsPG(t, ctx, storage, repo)
	var sourceRowsBefore int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_view_source_images WHERE repo_id=$1`, string(repo)).Scan(&sourceRowsBefore); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		old, err := storage.ReadPendingProjection(bound, repo)
		if err != nil || !old.Ready {
			t.Fatalf("initial MVCC ready read: %v", err)
		}
		for iteration := 0; iteration < 12; iteration++ {
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}',to_jsonb($2::text)) WHERE repo_id=$1`, string(repo), fmt.Sprintf("retained-version-%d", iteration))
			source := catalogReadStatePG(t, ctx, storage, repo)
			job = pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
			view, parts = pendingProjectionPartsPG(t, ctx, storage, job)
			if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
				t.Fatal(err)
			}
			pendingProjectionReadPG(t, ctx, storage, job, view)
			if retained := pendingProjectionCountsPG(t, ctx, storage, repo); retained != counts {
				t.Fatalf("replacement accumulated derived historical versions: initial=%v current=%v", counts, retained)
			}
			if current := catalogReadStatePG(t, ctx, storage, repo); current != source {
				t.Fatalf("derived GC changed source checkpoint: before=%+v after=%+v", source, current)
			}
		}
		stillOld, err := storage.ReadPendingProjection(bound, repo)
		if err != nil || !reflect.DeepEqual(stillOld, old) {
			t.Fatalf("derived GC invalidated an existing read snapshot: err=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var sourceRowsAfter int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_view_source_images WHERE repo_id=$1`, string(repo)).Scan(&sourceRowsAfter); err != nil || sourceRowsAfter != sourceRowsBefore+12 {
		t.Fatalf("derived GC removed original source history: before=%d after=%d err=%v", sourceRowsBefore, sourceRowsAfter, err)
	}
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM pending_contexts WHERE repo_id=$1`, string(repo))
	job = pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts = pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, job, view)
	if retained := pendingProjectionCountsPG(t, ctx, storage, repo); retained != [3]int64{counts[0] - 1, int64(len(parts)), 1} {
		t.Fatalf("deletion retained obsolete derived versions: %v", retained)
	}
	var tombstones int
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pending_projection_inputs WHERE repo_id=$1 AND deleted)
		+(SELECT count(*) FROM pending_projection_parts WHERE repo_id=$1 AND deleted)`, string(repo)).Scan(&tombstones); err != nil || tombstones != 0 {
		t.Fatalf("completed capture/publication retained tombstones: count=%d err=%v", tombstones, err)
	}
}
