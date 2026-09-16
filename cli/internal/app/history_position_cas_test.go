package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type conditionalPositionSelector interface {
	SelectPositionIfCurrent(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
}

type historyPositionCASFixture struct {
	root           string
	store          *storage.FileStore
	svc            *ContextHistoryService
	expected, next domain.WorkingPosition
	ref            domain.Ref
	memory         domain.ContentHash
}

func newHistoryPositionCASFixture(t *testing.T) historyPositionCASFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", commit)
	repo := string(domain.HashContent([]byte("history position CAS")))
	var ids []domain.ContentHash
	for _, label := range []string{"old selection", "promoted context"} {
		id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
		if len(ids) > 0 {
			snap.Parents = []domain.ContentHash{ids[0]}
		}
		if err := store.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, BranchID: domain.LegacyContextBranchID(repo, "main"), Target: ids[0]}
	if _, err := store.CreateBranchRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	svc := NewContextHistoryService(store, store)
	if err := svc.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", Snapshot: ids[0], GitCommit: commit, MemoryPinned: true}); err != nil {
		t.Fatal(err)
	}
	expected, err := svc.CurrentPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref.Target = ids[1]
	if err := store.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	memory, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: ids[1], Summary: "exact receipt memory"})
	if err != nil {
		t.Fatal(err)
	}
	next := domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: commit, Snapshot: ids[1], MemoryHash: memory, MemoryPinned: true}
	return historyPositionCASFixture{root, store, svc, expected, next, ref, memory}
}

func selectPositionCAS(t *testing.T, svc *ContextHistoryService, expected, next domain.WorkingPosition, ref domain.Ref) error {
	t.Helper()
	selector, ok := any(svc).(conditionalPositionSelector)
	if !ok {
		t.Fatal("history service lacks conditional position selection")
	}
	return selector.SelectPositionIfCurrent(context.Background(), expected, next, ref)
}

func TestSelectPositionIfCurrentPinsExactMemoryAndRecordsTransition(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded-memory", true: "recorded-empty-memory"}[empty], func(t *testing.T) {
			f := newHistoryPositionCASFixture(t)
			ctx := context.Background()
			newer, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.next.Snapshot, Summary: "later mutable memory"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.CompareAndSwapSnapshotMemory(ctx, f.next.Snapshot, "", newer); err != nil {
				t.Fatal(err)
			}
			if empty {
				f.next.MemoryHash = ""
			}
			if err := selectPositionCAS(t, f.svc, f.expected, f.next, f.ref); err != nil {
				t.Fatal(err)
			}
			p, err := f.svc.CurrentPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if p.Snapshot != f.next.Snapshot || p.SharedTarget != f.ref.Target || p.MemoryHash != f.next.MemoryHash || !p.MemoryPinned || p.Rewound || p.WorktreeID != f.expected.WorktreeID || p.BranchID != f.ref.BranchID {
				t.Fatalf("wrong prepared selection: %+v", p)
			}
			if p.Selection == nil || p.Selection.ID == f.expected.Selection.ID || p.Selection.Source != f.expected.Snapshot || p.Selection.Target != f.next.Snapshot || p.Selection.MemoryHash != p.MemoryHash || !p.Selection.MemoryPinned || p.Selection.GitBefore != f.expected.GitCommit || p.Selection.GitAfter != f.next.GitCommit {
				t.Fatalf("missing exact transition: %+v", p.Selection)
			}
			gotRef, err := f.store.GetRef(ctx, f.ref.RepoID, f.ref.Kind, f.ref.Name)
			if err != nil || gotRef != f.ref {
				t.Fatalf("shared ref changed: %+v %v", gotRef, err)
			}
		})
	}
}

func TestSelectPositionIfCurrentRefreshesSameSnapshotSharedBaseline(t *testing.T) {
	f := newHistoryPositionCASFixture(t)
	// The caller supplies eligibility; a conditional refresh must not be lost
	// to SelectPosition's ordinary same-snapshot replay shortcut.
	f.next.Snapshot, f.next.MemoryHash = f.expected.Snapshot, ""
	if err := selectPositionCAS(t, f.svc, f.expected, f.next, f.ref); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.CurrentPosition(context.Background())
	if err != nil || got.SharedTarget != f.ref.Target || !got.Rewound || got.Selection.ID == f.expected.Selection.ID {
		t.Fatalf("baseline not conditionally refreshed: %+v %v", got, err)
	}
}

type positionValidationInterleavingStore struct {
	*storage.FileStore
	interleave func()
}

func (s *positionValidationInterleavingStore) GetMemory(ctx context.Context, id domain.ContentHash) (domain.MemoryDigest, error) {
	digest, err := s.FileStore.GetMemory(ctx, id)
	if s.interleave != nil {
		call := s.interleave
		s.interleave = nil
		call()
	}
	return digest, err
}

func TestSelectPositionIfCurrentRejectsChangeDuringValidation(t *testing.T) {
	for _, change := range []string{"capture", "manual-memory-pin", "shared-ref"} {
		t.Run(change, func(t *testing.T) {
			f := newHistoryPositionCASFixture(t)
			ctx := context.Background()
			wrapped := &positionValidationInterleavingStore{FileStore: f.store}
			wrapped.interleave = func() {
				if change == "shared-ref" {
					ref := f.ref
					ref.Target = f.expected.Snapshot
					if err := f.store.PutRef(ctx, ref); err != nil {
						t.Fatal(err)
					}
					return
				}
				p := f.expected
				if change == "capture" {
					p.Snapshot = f.next.Snapshot
				} else {
					p.MemorySource = f.next.Snapshot
					p.MemoryHash = f.memory
				}
				p.Selection = nil
				if err := f.store.PutWorkingPosition(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			svc := NewContextHistoryService(wrapped, wrapped)
			if err := selectPositionCAS(t, svc, f.expected, f.next, f.ref); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("%s during validation = %v", change, err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if change == "shared-ref" && !reflect.DeepEqual(got, f.expected) {
				t.Fatalf("stale ref changed selection: %+v", got)
			}
			if change != "shared-ref" && (got.Selection.ID == f.expected.Selection.ID || got.SharedTarget != f.expected.SharedTarget) {
				t.Fatalf("newer selection overwritten: %+v", got)
			}
		})
	}
}

func TestSelectPositionIfCurrentRejectsInvalidMemoryWithoutMutation(t *testing.T) {
	for _, failure := range []string{"corrupt-memory", "missing-memory", "wrong-owner", "foreign-snapshot", "corrupt-doc"} {
		t.Run(failure, func(t *testing.T) {
			f := newHistoryPositionCASFixture(t)
			ctx := context.Background()
			memoryPath := filepath.Join(f.root, ".cxt", "objects", "memories", strings.TrimPrefix(string(f.memory), "sha256:"))
			switch failure {
			case "corrupt-memory":
				if err := os.WriteFile(memoryPath, []byte(`{"summary":"corrupted"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-memory":
				if err := os.Remove(memoryPath); err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				f.next.Snapshot = f.expected.Snapshot
			case "foreign-snapshot":
				snap, err := f.store.GetSnapshot(ctx, f.next.Snapshot)
				if err != nil {
					t.Fatal(err)
				}
				snap.RepoID = string(domain.HashContent([]byte("different repo")))
				// A separate immutable conversation gives valid foreign content.
				id, err := f.store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "foreign"}}})
				if err != nil {
					t.Fatal(err)
				}
				snap.ID, snap.DocHash, snap.Parents = id, id, nil
				if err := f.store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
				f.next.Snapshot = id
			case "corrupt-doc":
				path := filepath.Join(f.root, ".cxt", "objects", "docs", strings.TrimPrefix(string(f.next.Snapshot), "sha256:"))
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := selectPositionCAS(t, f.svc, f.expected, f.next, f.ref); err == nil {
				t.Fatalf("accepted %s", failure)
			}
			got, err := f.svc.CurrentPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, f.expected) {
				t.Fatalf("invalid data moved position: %+v %v", got, err)
			}
		})
	}
}
