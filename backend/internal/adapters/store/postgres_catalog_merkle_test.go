//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func catalogMerklePagePG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, req domain.CatalogMerkleRequest) domain.CatalogMerklePage {
	t.Helper()
	page, err := s.CatalogMerkle(ctx, repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := page.Validate(repo, req); err != nil {
		t.Fatal("invalid Merkle page", err)
	}
	return page
}

func catalogMerkleCountsPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash) (int, int) {
	t.Helper()
	var nodes, roots int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_merkle_nodes WHERE repo_id=$1`, string(repo)).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_merkle_roots WHERE repo_id=$1`, string(repo)).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	return nodes, roots
}

func catalogMerkleBaselinePG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, page domain.CatalogMerklePage) map[domain.ContentHash]domain.CatalogMerkleNode {
	t.Helper()
	entries, cp := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	root, nodes, err := domain.BuildCatalogMerkle(repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.CatalogMerkleHash(root)
	if err != nil || hash != page.RootHash || root.Count != page.Count || cp != page.Checkpoint {
		t.Fatalf("Merkle/baseline mismatch: %v", err)
	}
	return nodes
}

func catalogMerklePreparedPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash) catalogMerkleBuild {
	t.Helper()
	st, err := s.catalogMerkleState(ctx, s.pool, repo)
	if err != nil {
		t.Fatal(err)
	}
	build, err := s.buildCatalogMerkle(ctx, repo, st)
	if err != nil {
		t.Fatal(err)
	}
	return build
}

func TestPGCatalogMerkleBaselineWarmDeltaDeletion(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	snap := initializationPGSnapshot(t, s, repo, "merkle-baseline")
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','merkle/tag',$2)`, string(repo), string(snap.ID))
	first := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	catalogMerkleBaselinePG(t, ctx, s, repo, first)
	n, roots := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 273 || roots != 1 {
		t.Fatalf("cold cache counts: %d/%d", n, roots)
	}

	// Prove warm root lookup does not enumerate the source journal.
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `LOCK TABLE repository_catalog_changes IN ACCESS EXCLUSIVE MODE`)
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	warm, err := s.CatalogMerkle(probe, repo, domain.CatalogMerkleRequest{Version: 1})
	cancel()
	if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if err != nil || !reflect.DeepEqual(warm, first) {
		t.Fatalf("warm root touched locked source journal or drifted: %v", err)
	}
	n2, roots2 := catalogMerkleCountsPG(t, ctx, s, repo)
	if n2 != n || roots2 != roots {
		t.Fatal("unchanged root added cache records")
	}

	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message=U&'changed \D55C\AE00 <&>' WHERE repo_id=$1 AND id=$2`, string(repo), string(snap.ID))
	build := catalogMerklePreparedPG(t, ctx, s, repo)
	if len(build.payloads) != 3 {
		t.Fatalf("one changed key stages %d nodes, expected 3", len(build.payloads))
	}
	if err := s.publishCatalogMerkle(ctx, repo, build); err != nil {
		t.Fatal(err)
	}
	changed := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	if changed.RootHash == first.RootHash || changed.Checkpoint.Sequence <= first.Checkpoint.Sequence {
		t.Fatal("changed sequence/image did not change root")
	}
	catalogMerkleBaselinePG(t, ctx, s, repo, changed)
	n2, roots2 = catalogMerkleCountsPG(t, ctx, s, repo)
	if n2-n != 3 || roots2-roots != 1 {
		t.Fatalf("one-key growth=%d/%d", n2-n, roots2-roots)
	}

	catalogExecPG(t, ctx, s.pool, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name='merkle/tag'`, string(repo))
	deleted := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	if deleted.Count != changed.Count-1 {
		t.Fatal("tombstone did not remove exactly one live entry")
	}
	catalogMerkleBaselinePG(t, ctx, s, repo, deleted)
	n3, _ := catalogMerkleCountsPG(t, ctx, s, repo)
	if n3-n2 > 3 {
		t.Fatal("deletion rewrote untouched buckets")
	}
	old := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1, RootHash: first.RootHash, Checkpoint: &first.Checkpoint})
	if !reflect.DeepEqual(old, first) {
		t.Fatal("old root changed after delta/deletion")
	}
}

func TestPGCatalogMerkleFrozenLeafPaginationAcrossWrites(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	snap := initializationPGSnapshot(t, s, repo, "merkle-pages")
	// Select eight distinct typed keys sharing a leaf, without relying on hash luck.
	var bucket string
	var names []string
	for i := 0; len(names) < 8; i++ {
		name := fmt.Sprintf("merkle-page-%d", i)
		key, _ := json.Marshal([2]string{"tag", name})
		b, err := domain.CatalogMerkleBucket(domain.CatalogEntry{Kind: "ref", Key: string(key)})
		if err != nil {
			t.Fatal(err)
		}
		if bucket == "" {
			bucket = b
		}
		if b == bucket {
			names = append(names, name)
		}
	}
	tx := catalogTxPG(t, ctx, s)
	for _, name := range names {
		catalogExecPG(t, ctx, tx, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag',$2,$3)`, string(repo), name, string(snap.ID))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	root := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	nodes := catalogMerkleBaselinePG(t, ctx, s, repo, root)
	req := domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &root.Checkpoint, Prefix: bucket, Limit: 2}
	first := catalogMerklePagePG(t, ctx, s, repo, req)
	expected := nodes[first.NodeHash]
	if first.NextOffset == nil {
		t.Fatal("fixture did not create partial leaf")
	}
	catalogExecPG(t, ctx, s.pool, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name=$2`, string(repo), names[0])
	newer := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	if newer.RootHash == root.RootHash {
		t.Fatal("concurrent committed write was not observable")
	}
	got := append([]domain.CatalogEntry{}, first.Entries...)
	page := first
	for page.NextOffset != nil {
		req.Offset = *page.NextOffset
		page = catalogMerklePagePG(t, ctx, s, repo, req)
		if page.NodeHash != first.NodeHash || page.Checkpoint != root.Checkpoint || page.Count != first.Count {
			t.Fatal("frozen leaf drifted")
		}
		got = append(got, page.Entries...)
	}
	leaf, err := domain.NewCatalogMerkleLeaf(repo, bucket, got)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.CatalogMerkleHash(leaf)
	want, _ := catalogMerklePayload(expected)
	actual, _ := catalogMerklePayload(leaf)
	if err != nil || hash != first.NodeHash || !bytes.Equal(want, actual) {
		t.Fatal("frozen pages lost original entries", err)
	}
	req.Offset = int(first.Count)
	if _, err := s.CatalogMerkle(ctx, repo, req); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("accepted empty terminal replay", err)
	}
}

func TestPGCatalogMerkleBuildDoesNotWaitForSourceWriter(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	snap := initializationPGSnapshot(t, s, repo, "merkle-writer")
	committed := catalogReadStatePG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	if err := lockRepoGraph(ctx, tx, repo); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='still uncommitted' WHERE repo_id=$1 AND id=$2`, string(repo), string(snap.ID))
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	page, err := s.CatalogMerkle(probe, repo, domain.CatalogMerkleRequest{Version: 1})
	cancel()
	if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if err != nil || page.Checkpoint.Sequence != committed.head {
		t.Fatalf("build waited on source writer or exposed uncommitted state: %v", err)
	}
	catalogMerkleBaselinePG(t, ctx, s, repo, page)
}

func TestPGCatalogMerkleConcurrentBuildersAgree(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initializationPGSnapshot(t, s, repo, "merkle-concurrent")
	build := catalogMerklePreparedPG(t, ctx, s, repo)
	const workers = 4
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.publishCatalogMerkle(ctx, repo, build) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("equivalent builder conflict", err)
		}
	}
	n, roots := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 273 || roots != 1 {
		t.Fatalf("concurrent build records=%d/%d", n, roots)
	}
	root := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	catalogMerkleBaselinePG(t, ctx, s, repo, root)
	// Fresh actual entry-point calls must also agree on the next committed delta.
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='next generation' WHERE repo_id=$1`, string(repo))
	pages := make(chan domain.CatalogMerklePage, workers)
	failures := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := s.CatalogMerkle(ctx, repo, domain.CatalogMerkleRequest{Version: 1})
			if err != nil {
				failures <- err
			} else {
				pages <- page
			}
		}()
	}
	wg.Wait()
	close(pages)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var expected domain.CatalogMerklePage
	for page := range pages {
		if expected.RootHash != "" && !reflect.DeepEqual(page, expected) {
			t.Fatal("concurrent roots disagree")
		}
		expected = page
	}
	n, roots = catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 276 || roots != 2 {
		t.Fatalf("concurrent delta records=%d/%d", n, roots)
	}
}

func TestPGCatalogMerkleCorruptNodesNeverFallback(t *testing.T) {
	for _, mutation := range []string{"invalid_json", "scope", "noncanonical", "leaf", "branch"} {
		t.Run(mutation, func(t *testing.T) {
			s, ctx, repo := catalogPG(t)
			snap := initializationPGSnapshot(t, s, repo, "merkle-corrupt")
			root := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
			req := domain.CatalogMerkleRequest{Version: 1}
			hash := root.RootHash
			if mutation == "leaf" || mutation == "branch" {
				bucket, _ := domain.CatalogMerkleBucket(domain.CatalogEntry{Kind: "snapshot", Key: string(snap.ID)})
				prefix := bucket
				if mutation == "branch" {
					prefix = bucket[:1]
				}
				req = domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &root.Checkpoint, Prefix: prefix}
				page := catalogMerklePagePG(t, ctx, s, repo, req)
				hash = page.NodeHash
			}
			var raw []byte
			if err := s.pool.QueryRow(ctx, `SELECT payload FROM repository_catalog_merkle_nodes WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "scope":
				raw = bytes.Replace(raw, []byte(domain.CatalogMerkleScope), []byte("wrong-scope"), 1)
			case "noncanonical":
				raw = append(raw, ' ')
			default:
				raw = []byte("corrupt")
			}
			catalogExecPG(t, ctx, s.pool, `UPDATE repository_catalog_merkle_nodes SET payload=$3 WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash), raw)
			beforeNodes, beforeRoots := catalogMerkleCountsPG(t, ctx, s, repo)
			if _, err := s.CatalogMerkle(ctx, repo, req); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("corrupt cache accepted or silently rebuilt", err)
			}
			afterNodes, afterRoots := catalogMerkleCountsPG(t, ctx, s, repo)
			if beforeNodes != afterNodes || beforeRoots != afterRoots {
				t.Fatal("failed read changed cache")
			}
		})
	}
}

func TestPGCatalogMerkleImmutableCollisionRollsBack(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initializationPGSnapshot(t, s, repo, "merkle-conflict")
	build := catalogMerklePreparedPG(t, ctx, s, repo)
	// An existing same-hash/different-byte record may never be overwritten.
	catalogExecPG(t, ctx, s.pool, `INSERT INTO repository_catalog_merkle_nodes(repo_id,hash,payload) VALUES($1,$2,$3)`, string(repo), string(build.root), []byte("conflicting bytes"))
	if err := s.publishCatalogMerkle(ctx, repo, build); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("immutable collision accepted", err)
	}
	n, roots := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 1 || roots != 0 {
		t.Fatalf("partial publication survived rollback: %d/%d", n, roots)
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT payload FROM repository_catalog_merkle_nodes WHERE repo_id=$1 AND hash=$2`, string(repo), string(build.root)).Scan(&raw); err != nil || string(raw) != "conflicting bytes" {
		t.Fatal("collision payload changed", err)
	}
}

func TestPGCatalogMerkleRejectsWrongRootCheckpointAndRequest(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	root := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	cp := root.Checkpoint
	wrongRepo, wrongEpoch, future, wrongVersion := cp, cp, cp, cp
	wrongRepo.RepoID = domain.HashContent([]byte("other repo"))
	wrongEpoch.Epoch = "00000000-0000-0000-0000-000000000001"
	future.Sequence++
	wrongVersion.Version++
	tests := []struct {
		name string
		req  domain.CatalogMerkleRequest
		want error
	}{
		{"hash", domain.CatalogMerkleRequest{Version: 1, RootHash: domain.HashContent([]byte("wrong root")), Checkpoint: &cp}, domain.ErrCatalogResetRequired},
		{"repo", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &wrongRepo}, domain.ErrCatalogResetRequired},
		{"epoch", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &wrongEpoch}, domain.ErrCatalogResetRequired},
		{"future", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &future}, domain.ErrCatalogResetRequired},
		{"version", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &wrongVersion}, domain.ErrValidation},
		{"prefix", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash, Checkpoint: &cp, Prefix: "XX"}, domain.ErrValidation},
		{"no_root", domain.CatalogMerkleRequest{Version: 1, Prefix: "00"}, domain.ErrValidation},
		{"no_checkpoint", domain.CatalogMerkleRequest{Version: 1, RootHash: root.RootHash}, domain.ErrValidation},
		{"offset", domain.CatalogMerkleRequest{Version: 1, Offset: -1}, domain.ErrValidation},
		{"limit", domain.CatalogMerkleRequest{Version: 1, Limit: 1001}, domain.ErrValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.CatalogMerkle(ctx, repo, test.req); !errors.Is(err, test.want) {
				t.Fatal("wrong error", err)
			}
		})
	}
}

func TestPGCatalogMerkleResetPruneDuringBuildRejectsPublication(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			s, ctx, repo := catalogPG(t)
			initializationPGSnapshot(t, s, repo, "merkle-maintenance")
			old := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
			catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='next' WHERE repo_id=$1`, string(repo))
			staged := catalogMerklePreparedPG(t, ctx, s, repo)
			n, roots := catalogMerkleCountsPG(t, ctx, s, repo)
			if reset {
				catalogExecPG(t, ctx, s.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			} else {
				catalogExecPG(t, ctx, s.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), staged.checkpoint.Sequence)
			}
			if err := s.publishCatalogMerkle(ctx, repo, staged); !errors.Is(err, domain.ErrCatalogResetRequired) {
				t.Fatal("maintenance during build accepted", err)
			}
			n2, roots2 := catalogMerkleCountsPG(t, ctx, s, repo)
			if n2 != n || roots2 != roots {
				t.Fatal("failed maintenance check leaked cache records")
			}
			req := domain.CatalogMerkleRequest{Version: 1, RootHash: old.RootHash, Checkpoint: &old.Checkpoint}
			oldRead, err := s.CatalogMerkle(ctx, repo, req)
			if reset {
				if !errors.Is(err, domain.ErrCatalogResetRequired) {
					t.Fatal("old epoch served", err)
				}
			} else if err != nil || !reflect.DeepEqual(oldRead, old) {
				// Journal pruning is not immutable cache-node GC. Retained roots remain
				// readable within the same epoch, but may no longer seed a source delta.
				t.Fatal("pruning changed retained frozen root", err)
			}
			fresh := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
			catalogMerkleBaselinePG(t, ctx, s, repo, fresh)
			if fresh.RootHash == old.RootHash {
				t.Fatal("new sequence/image reused old root")
			}
		})
	}
}

func TestPGCatalogMerkleCancellationAndCallerTransactions(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.CatalogMerkle(canceled, repo, domain.CatalogMerkleRequest{Version: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	build := catalogMerklePreparedPG(t, ctx, s, repo)
	if err := s.publishCatalogMerkle(canceled, repo, build); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reject := func(bound context.Context) error {
		_, err := s.CatalogMerkle(bound, repo, domain.CatalogMerkleRequest{Version: 1})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("caller transaction escaped: %v", err)
		}
		if err := s.publishCatalogMerkle(bound, repo, build); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cache publication escaped: %v", err)
		}
		return nil
	}
	if err := s.WithinRepository(ctx, repo, reject); err != nil {
		t.Fatal(err)
	}
	if err := s.WithinReadSnapshot(ctx, reject); err != nil {
		t.Fatal(err)
	}
	reject(context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}}))
	n, roots := catalogMerkleCountsPG(t, ctx, s, repo)
	if n != 0 || roots != 0 {
		t.Fatal("rejected call published derived state")
	}
}

func TestCatalogMerklePayloadEquivalentJSON(t *testing.T) {
	repo := domain.HashContent([]byte("canonical payload fixture"))
	target := domain.HashContent([]byte("target"))
	keyA := "[\"tag\",\"\uD55C\uAE00<&>\"]"
	keyB := `[ "tag", "\uD55C\uAE00<\u0026>" ]`
	valueA := fmt.Sprintf("{\"repo_id\":%q,\"kind\":\"tag\",\"name\":\"\uD55C\uAE00<&>\",\"target\":%q}", repo, target)
	valueB := fmt.Sprintf(`{ "target":%q, "name":"\uD55C\uAE00<\u0026>", "kind":"tag", "repo_id":%q }`, target, repo)
	a := domain.CatalogEntry{Sequence: 7, Kind: "ref", Key: keyA, Value: json.RawMessage(valueA)}
	b := domain.CatalogEntry{Sequence: 7, Kind: "ref", Key: keyB, Value: json.RawMessage(valueB)}
	bucket, err := domain.CatalogMerkleBucket(a)
	if err != nil {
		t.Fatal(err)
	}
	leafA, err := domain.NewCatalogMerkleLeaf(repo, bucket, []domain.CatalogEntry{a})
	if err != nil {
		t.Fatal(err)
	}
	leafB, err := domain.NewCatalogMerkleLeaf(repo, bucket, []domain.CatalogEntry{b})
	if err != nil {
		t.Fatal(err)
	}
	rawA, err := catalogMerklePayload(leafA)
	if err != nil {
		t.Fatal(err)
	}
	rawB, err := catalogMerklePayload(leafB)
	if err != nil || !bytes.Equal(rawA, rawB) {
		t.Fatal("equivalent JSON has conflicting stored bytes", err)
	}
}
