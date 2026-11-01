//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func graphSourceDrainPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) ([]domain.RepositoryViewSourceEntry, domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	var entries []domain.RepositoryViewSourceEntry
	var initial domain.RepositoryViewSourceCheckpoint
	for pages := 0; pages < 4096; pages++ {
		page, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
		if err != nil {
			t.Fatal(err)
		}
		current := domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
		if current.Validate() != nil || current.Scope != domain.RepositoryGraphSourceScope || current.RepoID != repo || (pages > 0 && current != initial) {
			t.Fatalf("invalid or mixed source generation: %+v", current)
		}
		initial = current
		for _, entry := range page.Entries {
			if !domain.RepositoryGraphSourceKindValid(entry.Kind) || entry.Key == "" || entry.Sequence > current.Sequence || (entry.Deleted && len(entry.Value) != 0) || (!entry.Deleted && !json.Valid(entry.Value)) {
				t.Fatalf("invalid source entry: kind=%s key=%s", entry.Kind, entry.Key)
			}
			if len(entries) > 0 {
				last := entries[len(entries)-1]
				ordered := last.Kind < entry.Kind || (last.Kind == entry.Kind && last.Key < entry.Key)
				if page.Mode == "delta" {
					ordered = last.Sequence < entry.Sequence || (last.Sequence == entry.Sequence && ordered)
				}
				if !ordered {
					t.Fatal("source page duplicated or reordered a key")
				}
			}
			entries = append(entries, entry)
		}
		if page.NextCursor == "" {
			if page.Checkpoint == nil || *page.Checkpoint != current {
				t.Fatal("final source page did not publish the captured checkpoint")
			}
			return entries, current
		}
		if len(page.Entries) == 0 || page.Checkpoint != nil || page.NextCursor == request.Cursor {
			t.Fatal("source continuation did not progress")
		}
		request.After, request.Cursor = nil, page.NextCursor
	}
	t.Fatal("source page traversal exceeded fixture bound")
	return nil, domain.RepositoryViewSourceCheckpoint{}
}

func seedGraphSourcePG(t *testing.T, ctx context.Context, db catalogPGExecutor, repo domain.ContentHash) {
	t.Helper()
	const origin = "https://github.com/example/source"
	for _, query := range []string{
		`INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,'commit','{"commit":"before"}')`,
		`INSERT INTO git_tree_nodes(repo_id,origin,oid,payload) VALUES($1,$2,'tree','{"oid":"before"}')`,
		`INSERT INTO git_commit_deltas(repo_id,id,origin,commit_oid,parent_oid,payload,keys,inverse_keys) VALUES($1,'delta',$2,'commit','parent','{"delta":"before"}','{}','{}')`,
		`INSERT INTO git_ref_observations(repo_id,id,payload,observed_at) VALUES($1,'observation',jsonb_build_object('git_origin',$2::text,'ref','before'),now())`,
		`INSERT INTO pr_history_checkpoints(repo_id,origin,provider_repository_id,number,version,payload) VALUES($1,$2,1,7,1,'{"version":1,"pr":"before"}')`,
		`INSERT INTO pr_participations(repo_id,id,origin,provider_repository_id,number,payload) VALUES($1,repeat('a',64),$2,1,7,'{"capture":"before"}')`,
		`INSERT INTO pr_contributions(repo_id,id,payload) VALUES($1,repeat('b',64),jsonb_build_object('git_origin',$2::text,'capture','before'))`,
		`INSERT INTO git_contributions(repo_id,id,payload) VALUES($1,repeat('c',64),jsonb_build_object('git_origin',$2::text,'capture','before'))`,
		`INSERT INTO git_head_receipts(repo_id,origin,version,id,payload) VALUES($1,$2,1,repeat('d',64),'{"version":"1"}')`,
		`INSERT INTO git_contribution_admissions(repo_id,id,admission_key,origin,receipt_id,payload) VALUES($1,repeat('e',64),repeat('e',64),$2,repeat('d',64),'{"capture":"before"}')`,
		`INSERT INTO git_context_heads(repo_id,id,origin,destination_branch_id,destination_branch,read_version,receipt_id,payload) VALUES($1,repeat('f',64),$2,'main-id','main',1,repeat('d',64),'{"context_target":"before"}')`,
		`INSERT INTO git_contribution_proofs(repo_id,id,admission_id,context_head_id,contribution_id,payload) VALUES($1,repeat('1',64),repeat('e',64),repeat('f',64),repeat('c',64),jsonb_build_object('git_origin',$2::text,'contribution','before'))`,
	} {
		catalogExecPG(t, ctx, db, query, string(repo), origin)
	}
}

func TestPGRepositoryGraphSourceCoalescesProofsWithoutChangingLegacyScopes(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	_, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	viewBefore := repositoryViewSourceCheckpointPG(t, ctx, storage, repo)
	publicBefore := catalogCheckpointPG(t, ctx, storage, repo)
	tx := catalogTxPG(t, ctx, storage)
	seedGraphSourcePG(t, ctx, tx, repo)
	if current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || current != before {
		t.Fatalf("uncommitted evidence was exposed: %+v %v", current, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, after := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before, Limit: 1})
	if len(entries) != 12 || after.Epoch != before.Epoch || after.Sequence != before.Sequence+1 {
		t.Fatalf("proof transaction was not one generation: rows=%d before=%+v after=%+v", len(entries), before, after)
	}
	for _, entry := range entries {
		if entry.Sequence != after.Sequence || entry.Deleted || domain.RepositoryViewSourceKindValid(entry.Kind) {
			t.Fatalf("invalid proof generation: %s", entry.Kind)
		}
	}
	if current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || current != after {
		t.Fatalf("point generation disagrees with source: %+v %v", current, err)
	}
	viewDelta, viewAfter := repositoryViewSourceDeltaPG(t, ctx, storage, repo, viewBefore)
	if len(viewDelta) != 0 || viewAfter.Sequence != after.Sequence {
		t.Fatal("Git evidence leaked into legacy view scope")
	}
	public := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicBefore})
	if len(public.Entries) != 0 || public.Checkpoint == nil || public.Checkpoint.Sequence != after.Sequence {
		t.Fatal("Git evidence leaked into public catalog or did not advance checkpoint")
	}
	unchanged := after
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_commit_deltas SET keys=ARRAY['search-only'] WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_ref_observations SET observed_at=now() WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_tree_nodes SET payload=payload WHERE repo_id=$1`, string(repo))
	if current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || current != unchanged {
		t.Fatalf("nonsemantic metadata invalidated graph source: %+v %v", current, err)
	}
}

func TestPGRepositoryGraphSourceFixedImagesSurviveMutationAndDeletion(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	seedGraphSourcePG(t, ctx, storage.pool, repo)
	want, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	first, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("missing baseline continuation: %v", err)
	}
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `UPDATE pr_history_checkpoints SET version=2,payload='{"version":2,"pr":"after"}' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE git_ref_observations SET payload='{"ref":"after"}' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `DELETE FROM git_head_receipts WHERE repo_id=$1`, string(repo))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rest, fixed := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: first.NextCursor, Limit: 1})
	got := append(first.Entries, rest...)
	if !reflect.DeepEqual(got, want) || fixed != before {
		t.Fatal("fixed source baseline borrowed newer mutable proof rows")
	}
	delta, after := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before, Limit: 1})
	if len(delta) != 6 || after.Sequence != before.Sequence+1 {
		t.Fatalf("expected two updates and four cascaded tombstones, got %d", len(delta))
	}
	deleted := map[string]bool{}
	for _, entry := range delta {
		if entry.Deleted {
			deleted[entry.Kind] = true
		}
	}
	for _, kind := range []string{"git_receipt", "git_admission", "git_context_head", "git_contribution_proof"} {
		if !deleted[kind] {
			t.Fatalf("missing cascade tombstone for %s", kind)
		}
	}
}

func TestPGRepositoryGraphSourceRollbackAndRowMove(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	_, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	tx := catalogTxPG(t, ctx, storage)
	seedGraphSourcePG(t, ctx, tx, repo)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || current != before {
		t.Fatalf("rollback leaked a generation: %+v %v", current, err)
	}
	seedGraphSourcePG(t, ctx, storage.pool, repo)
	_, before = graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	tx = catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `UPDATE git_tree_nodes SET oid='moved' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `SAVEPOINT partial`)
	catalogExecPG(t, ctx, tx, `UPDATE git_tree_nodes SET payload='{"oid":"aborted"}' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `ROLLBACK TO SAVEPOINT partial`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	delta, _ := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before})
	if len(delta) != 2 || delta[0].Deleted || !delta[1].Deleted || strings.Contains(string(delta[0].Value), "aborted") {
		t.Fatal("row move/savepoint lost its new image or old-key tombstone")
	}
}

func TestPGRepositoryGraphSourceBoundsAndIdentity(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	seedGraphSourcePG(t, ctx, storage.pool, repo)
	_, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_tree_nodes SET payload=jsonb_build_object('large',repeat('x',$2)) WHERE repo_id=$1`, string(repo), domain.MaxRepositoryViewSourceImageBytes+1)
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before})
	if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
		t.Fatalf("oversized image returned partial data: %v", err)
	}
	if _, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil {
		t.Fatalf("metadata-only generation decoded an oversized proof: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.ReadRepositoryGraphSourceGeneration(cancelled, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("generation ignored cancellation: %v", err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(repo))
	if _, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("mismatched source epoch accepted: %v", err)
	}
	if _, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("mismatched source epoch captured: %v", err)
	}
}

func TestPGRepositoryGraphSourceDriverTransferIsPageBounded(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	_, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		SELECT $1,'https://github.com/example/source',lpad(number::text,3,'0'),jsonb_build_object('entries',repeat('x',700000))
		FROM generate_series(1,16) number`, string(repo))
	tx, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(tx)
	observed := &repositoryViewSourceBudgetTx{Tx: tx}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: observed, owner: storage, readOnly: true})
	page, err := storage.RepositoryGraphSourceChanges(bound, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before, Limit: 100})
	if err != nil || page.NextCursor == "" || len(page.Entries) != 5 || len(observed.queries) != 2 {
		t.Fatalf("bounded source page failed: entries=%d queries=%d err=%v", len(page.Entries), len(observed.queries), err)
	}
	var transferred, images int
	for _, query := range observed.queries {
		transferred += query.valueBytes
		images += query.images
	}
	if images != len(page.Entries) || transferred > domain.MaxRepositoryViewSourcePageBytes+64<<10 || observed.queries[0].images != 0 {
		t.Fatalf("driver drained unselected images: images=%d bytes=%d", images, transferred)
	}
}

func TestPGRepositoryGraphSourceGenerationUsesCurrentIdentityPolicy(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	legacy := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy}, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if _, err := storage.ReadRepositoryGraphSourceGeneration(legacy, repo); err == nil {
		t.Fatal("cached generation bypassed identity policy")
	}
	if _, err := storage.RepositoryGraphSourceChanges(legacy, repo, domain.RepositoryViewSourceRequest{Version: 1}); err == nil {
		t.Fatal("fixed images bypassed identity policy")
	}
	if _, err := storage.ReadRepositoryGraphSourceGeneration(compatible, repo); err != nil {
		t.Fatal(err)
	}
}

func TestPGRepositoryGraphSourceSerializesEvidenceWithViewWrites(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	_, before := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	first, second := catalogTxPG(t, ctx, storage), catalogTxPG(t, ctx, storage)
	var firstPID, secondPID int
	if err := first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, first, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload) VALUES($1,'https://github.com/example/source','tree','{}')`, string(repo))
	writer, cancel := context.WithTimeout(ctx, 5*time.Second)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := second.Exec(writer, `UPDATE repos SET default_branch='trunk' WHERE id=$1`, string(repo))
		done <- err
	}()
	defer func() {
		cancel()
		_ = first.Rollback(context.Background())
		<-done
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := storage.pool.QueryRow(writer, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, firstPID, secondPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("view mutation escaped uncommitted evidence: %v", err)
		case <-writer.Done():
			t.Fatal(writer.Err())
		case <-tick.C:
		}
	}
	if current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || current != before {
		t.Fatalf("in-flight writer exposed generation: %+v %v", current, err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	delta, after := graphSourceDrainPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &before, Limit: 1})
	if len(delta) != 2 || delta[0].Kind != "git_tree" || delta[1].Kind != "repository" || delta[0].Sequence != before.Sequence+1 || delta[1].Sequence != before.Sequence+2 || after.Sequence != before.Sequence+2 {
		t.Fatal("evidence and view generations did not follow commit order")
	}
}
