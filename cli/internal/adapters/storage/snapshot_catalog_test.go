package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func catalogSnapshot(seed string) domain.Snapshot {
	id := domain.HashContent([]byte(seed))
	return domain.Snapshot{
		ID: id, DocHash: id, RepoID: "repo", Branch: "main",
		Parents:        []domain.ContentHash{domain.HashContent([]byte("parent"))},
		MemoryHash:     domain.HashContent([]byte("memory")),
		ClaudeSettings: domain.HashContent([]byte("claude-settings")),
		AgentsSettings: domain.HashContent([]byte("agents-settings")),
		CodexSettings:  domain.HashContent([]byte("codex-settings")),
		Provider:       domain.ProviderCodex, Fidelity: domain.FidelityFull,
		Message: seed, Author: domain.TeamIdentity{Name: "Author", Email: "author@example.test", Team: "team"},
		CreatedAt:    time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Grafted:      true,
		GraftParents: []domain.ContentHash{domain.HashContent([]byte("graft"))}, GraftSeq: 3,
		SessionID: "session", Models: []string{"model-a", "model-b"}, CompactionCount: 2,
	}
}

// Fixtures contain metadata only: enumeration must not need document, memory,
// settings, or parent objects, and must not create mutation infrastructure.
func writeCatalogSnapshot(t *testing.T, store *FileStore, snap domain.Snapshot) {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	path := store.objectPath("snapshots", snap.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func catalogByID(t *testing.T, snapshots []domain.Snapshot) map[domain.ContentHash]domain.Snapshot {
	t.Helper()
	out := make(map[domain.ContentHash]domain.Snapshot, len(snapshots))
	for _, snap := range snapshots {
		if _, exists := out[snap.ID]; exists {
			t.Fatalf("duplicate snapshot %s", snap.ID)
		}
		out[snap.ID] = snap
	}
	return out
}

func catalogReaders(store *FileStore) map[string]func(context.Context) ([]domain.Snapshot, error) {
	return map[string]func(context.Context) ([]domain.Snapshot, error){
		"catalog": func(ctx context.Context) ([]domain.Snapshot, error) {
			return store.ListSnapshotCatalog(ctx, "repo")
		},
		"legacy": func(ctx context.Context) ([]domain.Snapshot, error) {
			return store.ListSnapshots(ctx, "repo", "")
		},
		"filtered": func(ctx context.Context) ([]domain.Snapshot, error) {
			return store.ListSnapshots(ctx, "repo", "main")
		},
	}
}

func catalogTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[path+"/"] = ""
			return nil
		}
		raw, err := os.ReadFile(path)
		tree[path] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestSnapshotCatalogMatchesLegacyMetadata(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	oldest, middle, newest := catalogSnapshot("oldest"), catalogSnapshot("middle"), catalogSnapshot("newest")
	middle.CreatedAt = oldest.CreatedAt.Add(time.Hour)
	middle.Branch = "feature"
	newest.CreatedAt = oldest.CreatedAt.Add(2 * time.Hour)
	// The store enumerates the whole replica. Application queries enforce scope.
	newest.RepoID = "another-repo"
	for _, snap := range []domain.Snapshot{middle, oldest, newest} {
		writeCatalogSnapshot(t, store, snap)
	}
	dir := filepath.Dir(store.objectPath("snapshots", oldest.ID))
	for _, name := range []string{".incomplete", "not-an-object"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ignored"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, strings.Repeat("0", 64)), 0o755); err != nil {
		t.Fatal(err)
	}
	before := catalogTree(t, store.repoRoot)
	want := []domain.Snapshot{newest, middle, oldest}
	for _, repo := range []string{"repo", "another-repo", ""} {
		got, err := store.ListSnapshotCatalog(ctx, repo)
		if err != nil || !reflect.DeepEqual(catalogByID(t, got), catalogByID(t, want)) {
			t.Fatalf("catalog(%q) = %+v, %v; want complete metadata", repo, got, err)
		}
		legacy, err := store.ListSnapshots(ctx, repo, "")
		if err != nil || !reflect.DeepEqual(legacy, want) {
			t.Fatalf("legacy(%q) = %+v, %v; want latest first", repo, legacy, err)
		}
	}
	for branch, want := range map[string][]domain.Snapshot{
		"main": {newest, oldest}, "feature": {middle}, "absent": nil,
	} {
		got, err := store.ListSnapshots(ctx, "repo", branch)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("branch %q = %+v, %v; want %+v", branch, got, err, want)
		}
	}
	if after := catalogTree(t, store.repoRoot); !reflect.DeepEqual(after, before) {
		t.Fatal("enumeration changed the store")
	}
}

func TestSnapshotCatalogFreshMetadata(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	snap := catalogSnapshot("mutable metadata")
	writeCatalogSnapshot(t, store, snap)
	read := func(want ...domain.Snapshot) []domain.Snapshot {
		t.Helper()
		got, err := store.ListSnapshotCatalog(ctx, "repo")
		if err != nil || !reflect.DeepEqual(catalogByID(t, got), catalogByID(t, want)) {
			t.Fatalf("catalog = %+v, %v; want %+v", got, err, want)
		}
		return got
	}
	first := read(snap)
	first[0].Message = "caller mutation"
	first[0].Parents[0] = "caller mutation"
	first[0].GraftParents[0] = "caller mutation"
	first[0].Models[0] = "caller mutation"
	read(snap)

	// Independent writers can update attachments without changing the object ID.
	writer := NewFileStore(store.repoRoot)
	next, err := writer.PutMemory(ctx, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "new projection"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CompareAndSwapSnapshotMemory(ctx, snap.ID, snap.MemoryHash, next); err != nil {
		t.Fatal(err)
	}
	snap.MemoryHash = next
	snap.GraftParents = nil
	snap.Grafted = false
	snap.GraftSeq++
	if err := writer.ReconcileGraftState(ctx, snap); err != nil {
		t.Fatal(err)
	}
	added := catalogSnapshot("new object")
	writeCatalogSnapshot(t, writer, added)
	read(snap, added)
	if err := os.Remove(store.objectPath("snapshots", snap.ID)); err != nil {
		t.Fatal(err)
	}
	read(added)
}

func TestSnapshotCatalogMissingStoreDoesNotWrite(t *testing.T) {
	for _, path := range []string{"", ".cxt", ".cxt/objects", ".cxt/objects/snapshots"} {
		t.Run("existing="+path, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			if path != "" {
				if err := os.MkdirAll(filepath.Join(store.repoRoot, path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before := catalogTree(t, store.repoRoot)
			for name, read := range catalogReaders(store) {
				got, err := read(context.Background())
				if err != nil || got != nil {
					t.Fatalf("%s = %+v, %v; want nil, nil", name, got, err)
				}
			}
			if after := catalogTree(t, store.repoRoot); !reflect.DeepEqual(after, before) {
				t.Fatal("empty enumeration created files or directories")
			}
		})
	}
}

func TestSnapshotCatalogCanceledBeforeScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			snap := catalogSnapshot("canceled")
			if corrupt {
				writeCatalogSnapshot(t, store, snap)
				if err := os.WriteFile(store.objectPath("snapshots", snap.ID), []byte("invalid json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := catalogTree(t, store.repoRoot)
			for name, read := range catalogReaders(store) {
				got, err := read(ctx)
				if !errors.Is(err, context.Canceled) || got != nil {
					t.Fatalf("%s = %+v, %v; want nil, context.Canceled", name, got, err)
				}
			}
			if got, err := store.GetSnapshot(ctx, snap.ID); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, domain.Snapshot{}) {
				t.Fatalf("GetSnapshot = %+v, %v; want zero snapshot, context.Canceled", got, err)
			}
			if after := catalogTree(t, store.repoRoot); !reflect.DeepEqual(after, before) {
				t.Fatal("canceled enumeration changed the store")
			}
		})
	}
}

// Cancel at an observed checkpoint rather than racing filesystem speed. The
// test sweeps every checkpoint and does not depend on an exact polling count.
type catalogCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int64
	checks   atomic.Int64
}

func (c *catalogCancelContext) Err() error {
	if c.checks.Add(1) == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func TestSnapshotCatalogCancellationDuringScan(t *testing.T) {
	store := NewFileStore(t.TempDir())
	const count = 5
	for i := range count {
		writeCatalogSnapshot(t, store, catalogSnapshot(fmt.Sprintf("snapshot-%d", i)))
	}
	for name, read := range catalogReaders(store) {
		t.Run(name, func(t *testing.T) {
			probe := &catalogCancelContext{Context: context.Background()}
			got, err := read(probe)
			if err != nil || len(got) != count {
				t.Fatalf("uncanceled read = %+v, %v", got, err)
			}
			checks := probe.checks.Load()
			if checks < count+2 {
				t.Fatalf("only %d cancellation checkpoints for %d objects", checks, count)
			}
			for checkpoint := int64(2); checkpoint <= checks; checkpoint++ {
				t.Run(fmt.Sprintf("checkpoint-%d", checkpoint), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					got, err := read(&catalogCancelContext{Context: ctx, cancel: cancel, cancelAt: checkpoint})
					if !errors.Is(err, context.Canceled) || got != nil {
						t.Fatalf("canceled read = %+v, %v; want no partial results", got, err)
					}
				})
			}
		})
	}
}

func TestSnapshotCatalogRejectsInvalidMetadata(t *testing.T) {
	for _, field := range []string{"json", "id", "doc", "memory", "claude", "agents", "codex", "parent", "graft-parent", "graft-seq"} {
		t.Run(field, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			good := catalogSnapshot("valid first")
			good.ID = domain.ContentHash("sha256:" + strings.Repeat("1", 64))
			good.DocHash = good.ID
			writeCatalogSnapshot(t, store, good)
			bad := catalogSnapshot("invalid other branch and repo")
			bad.ID = domain.ContentHash("sha256:" + strings.Repeat("e", 64))
			bad.DocHash = bad.ID
			bad.RepoID, bad.Branch = "another-repo", "feature"
			path := store.objectPath("snapshots", bad.ID)
			switch field {
			case "id":
				bad.ID, bad.DocHash = good.ID, good.ID
			case "doc":
				bad.DocHash = good.ID
			case "memory":
				bad.MemoryHash = "invalid"
			case "claude":
				bad.ClaudeSettings = "invalid"
			case "agents":
				bad.AgentsSettings = "invalid"
			case "codex":
				bad.CodexSettings = "invalid"
			case "parent":
				bad.Parents = []domain.ContentHash{"invalid"}
			case "graft-parent":
				bad.GraftParents = []domain.ContentHash{"invalid"}
			case "graft-seq":
				bad.GraftSeq = domain.MaxGraftSeq + 1
			}
			raw, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			if field == "json" {
				raw = []byte("invalid json")
			}
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			for name, read := range catalogReaders(store) {
				got, err := read(context.Background())
				if err == nil || got != nil {
					t.Fatalf("%s = %+v, %v; want failure without partial results", name, got, err)
				}
				if field != "json" && !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("%s error = %v; want ErrHashMismatch", name, err)
				}
			}
		})
	}
}

func TestSnapshotCatalogRejectsSymlinks(t *testing.T) {
	for _, target := range []string{".cxt", ".cxt/objects", ".cxt/objects/snapshots", "object"} {
		t.Run(target, func(t *testing.T) {
			store := NewFileStore(t.TempDir())
			snap := catalogSnapshot("symlink")
			writeCatalogSnapshot(t, store, snap)
			path := filepath.Join(store.repoRoot, target)
			if target == "object" {
				path = store.objectPath("snapshots", snap.ID)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			for name, read := range catalogReaders(store) {
				got, err := read(context.Background())
				if !errors.Is(err, domain.ErrHashMismatch) || got != nil {
					t.Fatalf("%s = %+v, %v; want nil, ErrHashMismatch", name, got, err)
				}
			}
		})
	}
}
