package backendclient

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Executed by e2e-sync against its owned server/database. Only a temporary local
// cache is modified; no server epoch, source metadata or active selection changes.
func TestCatalogMerkleLiveProtocol(t *testing.T) {
	base, token, repo := os.Getenv("CXT_MEMORY_REUSE_TEST_URL"), os.Getenv("CXT_MEMORY_REUSE_TEST_TOKEN"), os.Getenv("CXT_MEMORY_REUSE_TEST_REPO")
	if base == "" || token == "" || repo == "" {
		t.Skip("owned sync E2E server required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := NewBackendClient(func() string { return base }, func() string { return token }, domain.TeamIdentity{})
	cap, err := client.catalogCapability(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if cap.CatalogMerkleVersion == 0 {
		if os.Getenv("CXT_BRANCH_PULL_TEST_REQUIRED") == "1" {
			t.Fatal("PostgreSQL missing Merkle capability")
		}
		t.Skip("optional Merkle store unavailable")
	}
	cache := storage.NewFileStore(t.TempDir())
	client.SetCatalogCacheStore(cache)
	original, err := client.ReadSnapshotCatalog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	remote := client.SyncRemoteIdentity()
	state, err := cache.ReadCatalogCache(ctx, repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if state.Checkpoint == nil || len(original) == 0 {
		t.Fatal("missing baseline")
	}
	// Emulate an older managed replica with a valid complete image but a foreign
	// epoch and one absent snapshot. The real server must demand reset; the real
	// Merkle endpoint must supply the missing range and restore full parity.
	old := *state.Checkpoint
	old.Epoch = "00000000-0000-4000-8000-000000000001"
	if old.Epoch == state.Checkpoint.Epoch {
		old.Epoch = "00000000-0000-4000-8000-000000000002"
	}
	entries := make([]domain.CatalogEntry, 0, len(state.Entries)-1)
	removed := false
	for _, e := range state.Entries {
		if e.Kind == "snapshot" && !removed {
			removed = true
			continue
		}
		entries = append(entries, e)
	}
	revision, err := cache.InstallCatalogImage(ctx, state.Revision, repo, remote, old, entries)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := client.ReadSnapshotCatalog(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, reconciled) {
		t.Fatal("real Merkle wire differs from real catalog baseline")
	}
	after, err := cache.ReadCatalogCache(ctx, repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision == revision || after.Checkpoint.Epoch != state.Checkpoint.Epoch || len(after.Pending) != 0 {
		t.Fatal("incomplete/incorrect reconciliation publication")
	}
	observation, err := cache.ReadRemoteObservation(ctx, repo, remote)
	if err != nil || observation.Revision != "" {
		t.Fatal("metadata read acknowledged objects")
	}
	t.Logf("real catalog/Merkle parity: snapshots=%d metadata=%d epoch reset recovered; no server source mutation", len(reconciled), len(after.Entries))
}
