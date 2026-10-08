//go:build postgres

package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Metadata-only scale fixture. Body transfer/indexing is deliberately excluded;
// roots never claim that the referenced document bytes have been downloaded.
func TestPGCatalogRangesScale(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	first := initializationPGSnapshot(t, s, repo, "ranges-scale-one")
	second := initializationPGSnapshot(t, s, repo, "ranges-scale-two")
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) SELECT $1,'tag','scale/'||i,$2 FROM generate_series(1,10000) i`, string(repo), string(first.ID))
	start := time.Now()
	root := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	cold := time.Since(start)
	initialNodes, _ := catalogMerkleCountsPG(t, ctx, s, repo)
	start = time.Now()
	warm := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	warmTime := time.Since(start)
	if warm.RootHash != root.RootHash {
		t.Fatal("warm root differs")
	}
	start = time.Now()
	catalogExecPG(t, ctx, s.pool, `UPDATE refs SET target=$3 WHERE repo_id=$1 AND name=$2`, string(repo), "scale/1", string(second.ID))
	mutation := time.Since(start)
	start = time.Now()
	updated := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	updateTime := time.Since(start)
	nodes, _ := catalogMerkleCountsPG(t, ctx, s, repo)
	if nodes-initialNodes != 3 {
		t.Fatalf("changed key wrote %d nodes", nodes-initialNodes)
	}
	bucket, err := domain.CatalogMerkleBucket(domain.CatalogEntry{Kind: "ref", Key: `["tag","scale/1"]`})
	if err != nil {
		t.Fatal(err)
	}
	leaf := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1, RootHash: updated.RootHash, Checkpoint: &updated.Checkpoint, Prefix: bucket, Limit: 1000})
	if leaf.Count >= 100 || leaf.NextOffset != nil {
		t.Fatalf("unexpected bucket fanout %d", leaf.Count)
	}
	raw, _ := json.Marshal(root)
	leafRaw, _ := json.Marshal(leaf)
	entries, cp := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1000}), 1000)
	full, _, err := domain.BuildCatalogMerkle(repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	fullHash, err := domain.CatalogMerkleHash(full)
	if err != nil || fullHash != updated.RootHash || cp != updated.Checkpoint {
		t.Fatalf("baseline parity: %v", err)
	}
	t.Logf("metadata_count=%d cold_root=%s warm_root=%s source_update=%s delta_root=%s changed_nodes=%d changed_leaf_entries=%d root_response_bytes=%d leaf_response_bytes=%d", root.Count, cold, warmTime, mutation, updateTime, nodes-initialNodes, leaf.Count, len(raw), len(leafRaw))
}
