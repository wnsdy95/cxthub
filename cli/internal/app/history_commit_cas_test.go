package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type savePositionInterleavingStore struct {
	*storage.FileStore
	interleave func()
}

func (s *savePositionInterleavingStore) PutSnapshot(ctx context.Context, snapshot domain.Snapshot) error {
	if err := s.FileStore.PutSnapshot(ctx, snapshot); err != nil {
		return err
	}
	if s.interleave != nil {
		call := s.interleave
		s.interleave = nil
		call()
	}
	return nil
}

func TestSavePreservesPositionSelectedAfterReadingBaseline(t *testing.T) {
	for _, change := range []string{"none", "memory-only", "same-code-selection"} {
		t.Run(change, func(t *testing.T) {
			f := newHistoryPositionCASFixture(t)
			ctx := context.Background()
			// There is no ref race: only the worktree-local selection changes.
			ref := f.ref
			ref.Target = f.expected.Snapshot
			if err := f.store.PutRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			winner := f.expected
			store := &savePositionInterleavingStore{FileStore: f.store}
			if change != "none" {
				store.interleave = func() {
					p := f.expected
					if change == "memory-only" {
						memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "newly selected memory"})
						if err != nil {
							t.Fatal(err)
						}
						p.MemoryHash = memory
					}
					p.Selection = nil
					if err := f.store.PutWorkingPosition(ctx, p); err != nil {
						t.Fatal(err)
					}
					var err error
					winner, err = f.store.GetWorkingPosition(ctx)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			path := filepath.Join(f.root, "session.jsonl")
			native := strings.ReplaceAll(e2eClaudeSession, "/Users/work/proj", f.root)
			if err := os.WriteFile(path, []byte(native), 0600); err != nil {
				t.Fatal(err)
			}
			repo := domain.Repo{ID: f.ref.RepoID, LocalPath: f.root, DefaultBranch: "main"}
			svc := newTestSaveService(pushOrderGit{repo}, map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}, store)
			out, err := svc.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: domain.ProviderClaude, SessionPath: path})
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
				p, err := f.store.GetWorkingPosition(ctx)
				if err != nil || p.Snapshot != out.SnapshotID || p.SharedTarget != out.SnapshotID {
					t.Fatalf("ordinary save failed: %+v %v", p, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("Save overwrote %s: output=%+v err=%v", change, out, err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, winner) {
				t.Fatalf("selection winner lost: %+v want %+v, %v", got, winner, err)
			}
			gotRef, err := f.store.GetRef(ctx, ref.RepoID, ref.Kind, ref.Name)
			if err != nil || gotRef != ref {
				t.Fatalf("stale save advanced ref: %+v %v", gotRef, err)
			}
			if _, err := os.Stat(filepath.Join(f.root, ".cxt", "working-commit.json")); !os.IsNotExist(err) {
				t.Fatalf("stale save journaled: %v", err)
			}
		})
	}
}

func TestPendingCaptureSurvivesStaleSharedBranchSelection(t *testing.T) {
	f := newHistoryPositionCASFixture(t)
	ctx := context.Background()
	// The historical position was selected before the shared branch advanced.
	old := f.expected
	old.Rewound = true
	if err := f.store.PutWorkingPosition(ctx, old); err != nil {
		t.Fatal(err)
	}
	old, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, "session.jsonl")
	native := strings.ReplaceAll(e2eClaudeSession, "/Users/work/proj", f.root)
	if err := os.WriteFile(path, []byte(native), 0600); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: f.ref.RepoID, LocalPath: f.root, DefaultBranch: "main"}
	svc := newTestSaveService(pushOrderGit{repo}, map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}, f.store)
	in := inbound.SaveInput{Cwd: f.root, Provider: domain.ProviderClaude, SessionPath: path, Pending: true, Message: domain.HookMessagePrefix + "capture"}
	out, err := svc.Save(ctx, in)
	if err != nil {
		t.Fatalf("private pending capture rejected: %v", err)
	}
	pending, err := f.store.ListPendings(ctx, repo.ID)
	if err != nil || len(pending) != 1 || pending[0].Target != out.SnapshotID {
		t.Fatalf("private bytes not retained: %+v %v", pending, err)
	}
	snap, err := f.store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil || len(snap.Parents) != 1 || snap.Parents[0] != old.Snapshot || len(snap.GraftParents) != 0 {
		t.Fatalf("pending lineage changed: %+v %v", snap, err)
	}
	// Publishing that same pending capture still requires resolving the stale
	// selection. The exception must not weaken the normal commit conflict.
	in.Pending, in.Message = false, "commit"
	if _, err := svc.Save(ctx, in); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale branch commit accepted: %v", err)
	}
	got, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(got, old) {
		t.Fatalf("pending capture moved worktree: %+v %v", got, err)
	}
	ref, err := f.store.GetRef(ctx, f.ref.RepoID, f.ref.Kind, f.ref.Name)
	if err != nil || ref != f.ref {
		t.Fatalf("pending capture moved shared ref: %+v %v", ref, err)
	}
	remaining, err := f.store.ListPendings(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(remaining, pending) {
		t.Fatalf("failed commit lost pending: %+v %v", remaining, err)
	}
}
