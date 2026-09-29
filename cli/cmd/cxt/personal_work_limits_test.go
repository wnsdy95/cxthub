package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPersonalWorkRejectsRecapturedSyntheticUserSeeds(t *testing.T) {
	for _, text := range []string{"[cxt seed] Branch-switch context: main → task\nold tasks", "[cxt] This session was resumed from a branch context seed.", "<environment_context>machine state</environment_context>", "prefix\n<environment_context cwd=\"/tmp\">machine state</environment_context>", "nested [cxt context package v1]\n{}"} {
		t.Run(text, func(t *testing.T) {
			cwd, a, f := personalFixture(t)
			f.doc.CIR.Events[0].Blocks[0].Text = text
			a.State.Constraints[0].Text = text
			rehashPersonalFixture(t, &a, f)
			path := writePersonalFixture(t, cwd, a)
			if _, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, path, domain.PersonalWorkScope{}); err == nil {
				t.Fatal("synthetic plain user event accepted")
			}
		})
	}
}

type limitedPersonalBackend struct {
	*personalBackendFixture
	docs      map[domain.ContentHash]domain.SessionDoc
	remaining []int64
	used      int64
}

func (f *limitedPersonalBackend) GetSnapshotRemote(_ context.Context, _ string, id domain.ContentHash) (domain.Snapshot, error) {
	s := f.snap
	s.ID, s.DocHash = id, id
	return s, nil
}
func (f *limitedPersonalBackend) FetchPersonalWorkDocument(_ context.Context, _ string, id domain.ContentHash, remaining int64) (domain.SessionDoc, int64, error) {
	f.remaining = append(f.remaining, remaining)
	doc := f.docs[id]
	raw, err := json.Marshal(doc)
	used := int64(len(raw))
	if f.used > 0 {
		used = f.used
	}
	return doc, used, err
}

func TestPersonalWorkCapsDocumentsAndCumulativeBytes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		documents int
		used      int64
		wantReads int
	}{
		{"document count", 9, 0, 8},
		{"cumulative bytes", 2, 5 << 20, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, a, base := personalFixture(t)
			f := &limitedPersonalBackend{personalBackendFixture: base, docs: map[domain.ContentHash]domain.SessionDoc{}, used: tc.used}
			a.State.Constraints = nil
			a.State.Sources = nil
			for i := 0; i < tc.documents; i++ {
				doc := base.doc
				doc.CIR.Events = []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("source %d", i)}}}}
				raw, err := domain.CanonicalBytes(doc.CIR)
				if err != nil {
					t.Fatal(err)
				}
				doc.Hash = domain.HashContent(raw)
				f.docs[doc.Hash] = doc
				a.State.Sources = append(a.State.Sources, domain.AgentSourcePointer{SnapshotID: doc.Hash, DocHash: doc.Hash, EndEvent: 1, Tool: "context_fetch"})
			}
			if _, _, err := importPersonalWork(context.Background(), f, a.RepositoryID, cwd, writePersonalFixture(t, cwd, a), domain.PersonalWorkScope{}); err == nil {
				t.Fatal("unbounded source import accepted")
			}
			if len(f.remaining) != tc.wantReads {
				t.Fatalf("reads=%v want %d", f.remaining, tc.wantReads)
			}
			if tc.used > 0 && (f.remaining[0] != 8<<20 || f.remaining[1] != 3<<20) {
				t.Fatalf("remaining allowance not passed to HTTP reader: %v", f.remaining)
			}
		})
	}
}

func TestPersonalWorkWorktreeIdentityMatchesStoreThroughSymlinks(t *testing.T) {
	ctx := context.Background()
	cwd, a, _ := personalFixture(t)
	if out, err := exec.Command("git", "-C", cwd, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("commit: %s %v", out, err)
	}
	linked := t.TempDir()
	if out, err := exec.Command("git", "-C", cwd, "-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", linked, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %s %v", out, err)
	}
	keys := map[string]bool{}
	for _, root := range []string{cwd, linked} {
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{root, alias} {
			key, err := personalWorktreeID(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := personalWorktreeID(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if key != expected {
				t.Fatal("symlink changed worktree identity")
			}
			scope := a.State.Scope
			scope.WorktreeID = key
			store := storage.NewWorktreeFileStore(cwd, configGitValue(path, "rev-parse", "--absolute-git-dir"), "main", "")
			if _, err := store.ReadPersonalWork(ctx, a.RepositoryID, scope); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("identity does not match FileStore: %v", err)
			}
			keys[key] = true
		}
	}
	if len(keys) != 2 {
		t.Fatal("linked worktrees share a personal identity")
	}
}
