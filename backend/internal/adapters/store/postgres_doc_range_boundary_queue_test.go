//go:build postgres

package store

import (
	"strings"
	"testing"
)

func TestDocRangeBoundaryPGActiveQueueIndexAndBound(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	var column, predicate string
	if err := storage.pool.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid,1,true),pg_get_expr(indpred,indrelid)
 FROM pg_index WHERE indexrelid='doc_range_jobs_v1_active_repo'::regclass`).Scan(&column, &predicate); err != nil {
		t.Fatal(err)
	}
	if column != "repo_id" || strings.Contains(predicate, "completed") {
		t.Fatal("unbounded queue index", column, predicate)
	}
	for _, state := range []string{"waiting", "running", "retrying"} {
		if !strings.Contains(predicate, state) {
			t.Fatal("active state missing from partial index", predicate)
		}
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_range_jobs_v1(repo_id,doc_hash,base_hash,state,created_at,next_attempt)
 SELECT $1,$1,'sha256:'||encode(digest(ordinal::text,'sha256'),'hex'),
 CASE WHEN ordinal<=64 THEN 'waiting' ELSE 'completed' END,clock_timestamp(),clock_timestamp()
 FROM generate_series(1,10000) ordinal`, repo); err != nil {
		t.Fatal(err)
	}
}
