package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func securityHash(c string) domain.ContentHash {
	return domain.ContentHash("sha256:" + strings.Repeat(c, 64))
}

func TestFileStoreRejectsTraversalSnapshotID(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	store := NewFileStore(repoRoot)

	err := store.PutSnapshot(ctx, domain.Snapshot{
		ID:       "sha256:../../../outside",
		DocHash:  "sha256:../../../outside",
		Branch:   "main",
		Provider: domain.ProviderClaude,
	})
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("expected ErrHashMismatch, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "outside")); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestFileStoreRejectsTraversalRefName(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	store := NewFileStore(repoRoot)

	err := store.PutRef(ctx, domain.Ref{
		Kind:   domain.RefBranch,
		Name:   "../../../../outside",
		Target: securityHash("1"),
	})
	if !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatalf("expected ErrInvalidRef, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "outside")); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestFileStoreRejectsTraversalDocHash(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	store := NewFileStore(repoRoot)

	_, err := store.GetDoc(ctx, "sha256:../../../outside")
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("expected ErrHashMismatch, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "outside")); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestFileStorePendingKeysDoNotCollapse(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	for i, sessionID := range []string{"terminal/a", "terminal?a"} {
		if err := store.PutPending(ctx, domain.Pending{SessionID: sessionID, Target: securityHash(string(rune('1' + i)))}); err != nil {
			t.Fatalf("PutPending(%q): %v", sessionID, err)
		}
	}
	pendings, err := store.ListPendings(ctx, "")
	if err != nil || len(pendings) != 2 {
		t.Fatalf("pending collision: len=%d err=%v", len(pendings), err)
	}
	if err := store.DeletePending(ctx, "", "terminal/a"); err != nil {
		t.Fatal(err)
	}
	pendings, err = store.ListPendings(ctx, "")
	if err != nil || len(pendings) != 1 || pendings[0].SessionID != "terminal?a" {
		t.Fatalf("deleting one pending affected another: %+v err=%v", pendings, err)
	}
}

func TestFileStoreCompareAndDeletePendingUsesTargetCAS(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	oldTarget := securityHash("1")
	newTarget := securityHash("2")
	const sessionID = "terminal/cas"

	if err := store.PutPending(ctx, domain.Pending{SessionID: sessionID, Target: newTarget}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.CompareAndDeletePending(ctx, "", sessionID, oldTarget)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("stale expected target deleted a newer pending capture")
	}
	pendings, err := store.ListPendings(ctx, "")
	if err != nil || len(pendings) != 1 || pendings[0].Target != newTarget {
		t.Fatalf("newer pending was not preserved: %+v err=%v", pendings, err)
	}

	deleted, err = store.CompareAndDeletePending(ctx, "", sessionID, newTarget)
	if err != nil || !deleted {
		t.Fatalf("matching target compare-delete: deleted=%v err=%v", deleted, err)
	}
	pendings, err = store.ListPendings(ctx, "")
	if err != nil || len(pendings) != 0 {
		t.Fatalf("matching pending remains: %+v err=%v", pendings, err)
	}

	deleted, err = store.CompareAndDeletePending(ctx, "", sessionID, newTarget)
	if err != nil || !deleted {
		t.Fatalf("absent pending must be an idempotent success: deleted=%v err=%v", deleted, err)
	}
}

func TestFileStoreReplacePendingReturnsLinearizedPredecessor(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	const sessionID = "terminal/replace"
	initial := securityHash("0")
	if err := store.PutPending(ctx, domain.Pending{SessionID: sessionID, Target: initial}); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	oldTargets := make(chan domain.ContentHash, writers)
	var wg sync.WaitGroup
	for i := 1; i <= writers; i++ {
		target := securityHash(string(rune('0' + i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			old, err := store.ReplacePending(ctx, domain.Pending{SessionID: sessionID, Target: target})
			if err != nil {
				t.Errorf("replace %s: %v", target, err)
				return
			}
			oldTargets <- old
		}()
	}
	wg.Wait()
	close(oldTargets)

	counts := make(map[domain.ContentHash]int, writers+1)
	for old := range oldTargets {
		counts[old]++
	}
	pendings, err := store.ListPendings(ctx, "")
	if err != nil || len(pendings) != 1 {
		t.Fatalf("final pending: %+v err=%v", pendings, err)
	}
	counts[pendings[0].Target]++
	for i := 0; i <= writers; i++ {
		target := securityHash(string(rune('0' + i)))
		if counts[target] != 1 {
			t.Fatalf("target %s occurs %d times across predecessors+tip; replacement was not linearized", target, counts[target])
		}
	}
}

func TestFileStoreRefusesSymlinkedCxtDirectories(t *testing.T) {
	ctx := context.Background()
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "objects"}[nested], func(t *testing.T) {
			repo := t.TempDir()
			outside := t.TempDir()
			if nested {
				if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(repo, ".cxt", "objects")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			store := NewFileStore(repo)
			if _, err := store.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("secret")}); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("symlinked store error = %v, want ErrHashMismatch", err)
			}
			outsideDocs := filepath.Join(outside, "docs")
			if !nested {
				outsideDocs = filepath.Join(outside, "objects", "docs")
			}
			if _, err := os.Stat(outsideDocs); !os.IsNotExist(err) {
				t.Fatalf("outside object directory created: %v", err)
			}
		})
	}
}

func TestFileStoreRefusesSymlinkedObjectReads(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store := NewFileStore(repo)
	doc := domain.SessionDoc{CIR: sampleCIR("verified")}
	hash, err := store.PutDoc(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	object := store.objectPath("docs", hash)
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, object); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.GetDoc(ctx, hash); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("symlinked object read error = %v, want ErrHashMismatch", err)
	}
}

func TestFileStoreRefusesDeleteThroughSymlinkedObjectDirectory(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt", "objects")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	hash := securityHash("d")
	docs := filepath.Join(outside, "docs")
	if err := os.Mkdir(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(docs, strings.TrimPrefix(string(hash), "sha256:"))
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(repo)
	if err := store.DeleteDoc(ctx, hash); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("delete through symlink error = %v, want ErrHashMismatch", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}
