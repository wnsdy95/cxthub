package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Keep this local so the regression fails at runtime before the optional port
// exists, without changing every WorkingPositionStore implementation.
type positionCAS interface {
	CompareAndSwapWorkingPosition(context.Context, domain.WorkingPosition, domain.WorkingPosition, domain.Ref) error
}

type positionCASFixture struct {
	store, peer                  *FileStore
	expected, next, peerPosition domain.WorkingPosition
	ref                          domain.Ref
}

func newPositionCASFixture(t *testing.T) positionCASFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	peer := NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "main", s.gitCommit)
	repo := string(domain.HashContent([]byte("position CAS repo")))
	var ids []domain.ContentHash
	for _, label := range []string{"old", "promoted"} {
		id, err := s.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
		if len(ids) > 0 {
			snap.Parents = []domain.ContentHash{ids[0]}
		}
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: domain.LegacyContextBranchID(repo, "main"), Target: ids[1]}
	if _, err := s.CreateBranchRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	old := domain.WorkingPosition{RepoID: repo, Branch: "main", BranchID: ref.BranchID, GitCommit: s.gitCommit, Snapshot: ids[0], SharedTarget: ids[0], MemoryPinned: true, Rewound: true}
	for _, owner := range []*FileStore{s, peer} {
		if err := owner.PutWorkingPosition(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := s.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	peerPosition, err := peer.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := expected
	next.Snapshot, next.SharedTarget, next.Rewound = ids[1], ids[1], false
	next.Selection = &domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: repo, BranchID: ref.BranchID, Branch: "main", Kind: "position", Source: ids[0], Target: ids[1], MemoryPinned: true, GitBefore: s.gitCommit, GitAfter: s.gitCommit, WorktreeID: s.worktreeID, CreatedAt: time.Now().UTC()}
	return positionCASFixture{s, peer, expected, next, peerPosition, ref}
}

func casPosition(t *testing.T, s *FileStore, expected, next domain.WorkingPosition, ref domain.Ref) error {
	t.Helper()
	cas, ok := any(s).(positionCAS)
	if !ok {
		t.Fatal("FileStore lacks atomic position/ref comparison")
	}
	return cas.CompareAndSwapWorkingPosition(context.Background(), expected, next, ref)
}

func TestWorkingPositionCASReconcilesOnlyOwnerAndRetainsSelection(t *testing.T) {
	f := newPositionCASFixture(t)
	if err := casPosition(t, f.store, f.expected, f.next, f.ref); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.GetWorkingPosition(context.Background())
	if err != nil || !reflect.DeepEqual(got, f.next) {
		t.Fatalf("position = %+v, %v; want %+v", got, err, f.next)
	}
	peer, err := f.peer.GetWorkingPosition(context.Background())
	if err != nil || !reflect.DeepEqual(peer, f.peerPosition) {
		t.Fatalf("peer moved: %+v %v", peer, err)
	}
	ref, err := f.store.GetRef(context.Background(), f.ref.RepoID, f.ref.Kind, f.ref.Name)
	if err != nil || ref != f.ref {
		t.Fatalf("branch ref changed: %+v %v", ref, err)
	}
	events, err := f.store.ListHistoryEvents(context.Background(), f.ref.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.ID] = true
	}
	if !seen[f.expected.Selection.ID] || !seen[f.next.Selection.ID] {
		t.Fatalf("lost selection history: %+v", events)
	}
	if err := casPosition(t, f.store, f.expected, f.next, f.ref); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale replay = %v", err)
	}
}

func TestWorkingPositionCASRejectsEveryStalePositionField(t *testing.T) {
	changes := map[string]func(*domain.WorkingPosition){
		"repo":             func(p *domain.WorkingPosition) { p.RepoID = string(domain.HashContent([]byte("different repo"))) },
		"snapshot":         func(p *domain.WorkingPosition) { p.Snapshot = domain.HashContent([]byte("another snapshot")) },
		"memory":           func(p *domain.WorkingPosition) { p.MemoryHash = domain.HashContent([]byte("new memory")) },
		"memory-source":    func(p *domain.WorkingPosition) { p.MemorySource = p.Snapshot },
		"memory-pin":       func(p *domain.WorkingPosition) { p.MemoryPinned = false },
		"shared-target":    func(p *domain.WorkingPosition) { p.SharedTarget = domain.HashContent([]byte("new shared")) },
		"rewound":          func(p *domain.WorkingPosition) { p.Rewound = false },
		"orphan":           func(p *domain.WorkingPosition) { p.Orphan = true },
		"branch":           func(p *domain.WorkingPosition) { p.Branch = "other" },
		"identity":         func(p *domain.WorkingPosition) { p.BranchID = "other-incarnation" },
		"local-branch":     func(p *domain.WorkingPosition) { p.LocalBranch = "alias" },
		"git-commit":       func(p *domain.WorkingPosition) { p.GitCommit = strings.Repeat("c", 40) },
		"selection-id":     func(p *domain.WorkingPosition) { p.Selection.ID = strings.Repeat("d", 32) },
		"selection-memory": func(p *domain.WorkingPosition) { p.Selection.MemoryPinned = false },
		"selection-time":   func(p *domain.WorkingPosition) { p.Selection.CreatedAt = p.Selection.CreatedAt.Add(time.Second) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newPositionCASFixture(t)
			current := f.expected
			selection := *current.Selection
			current.Selection = &selection
			change(&current)
			// Emulate an intervening persisted writer without normalizing fields.
			raw, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(f.store.positionPath(), raw); err != nil {
				t.Fatal(err)
			}
			if err := casPosition(t, f.store, f.expected, f.next, f.ref); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("stale %s = %v", name, err)
			}
			after, err := os.ReadFile(f.store.positionPath())
			if err != nil || string(after) != string(raw) {
				t.Fatalf("intervening position overwritten: %v", err)
			}
		})
	}
}

func TestWorkingPositionCASRejectsCorruptionAfterPreparation(t *testing.T) {
	for _, corrupt := range []string{"memory", "document"} {
		t.Run(corrupt, func(t *testing.T) {
			f := newPositionCASFixture(t)
			ctx := context.Background()
			memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.next.Snapshot, Summary: "selected memory"})
			if err != nil {
				t.Fatal(err)
			}
			f.next.MemoryHash, f.next.Selection.MemoryHash = memory, memory
			path := f.store.objectPath("memories", memory)
			if corrupt == "document" {
				path = f.store.objectPath("docs", f.next.Snapshot)
			}
			if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := casPosition(t, f.store, f.expected, f.next, f.ref); err == nil {
				t.Fatalf("accepted corrupt %s", corrupt)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, f.expected) {
				t.Fatalf("corruption moved selection: %+v %v", got, err)
			}
		})
	}
}

func TestWorkingPositionCASRejectsStaleRefAndStoreGitObservation(t *testing.T) {
	for _, kind := range []string{"ref-target", "ref-identity", "ref-missing", "ref-archived", "git-branch", "git-commit", "wrong-worktree"} {
		t.Run(kind, func(t *testing.T) {
			f := newPositionCASFixture(t)
			ctx := context.Background()
			writer := f.store
			switch kind {
			case "ref-target", "ref-identity":
				ref := f.ref
				if kind == "ref-target" {
					ref.Target = f.expected.Snapshot
				} else {
					ref.BranchID = "recreated"
				}
				if err := writer.PutRef(ctx, ref); err != nil {
					t.Fatal(err)
				}
			case "ref-missing":
				if err := os.Remove(writer.refPath("heads", "main")); err != nil {
					t.Fatal(err)
				}
			case "ref-archived":
				if _, err := writer.ArchiveBranchRef(ctx, f.ref.RepoID, "main"); err != nil {
					t.Fatal(err)
				}
			case "git-branch":
				copy := *writer
				copy.gitBranch = "other"
				writer = &copy
			case "git-commit":
				copy := *writer
				copy.gitCommit = strings.Repeat("c", 40)
				writer = &copy
			case "wrong-worktree":
				writer = f.peer
			}
			before, err := os.ReadFile(f.store.positionPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := casPosition(t, writer, f.expected, f.next, f.ref); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("%s = %v", kind, err)
			}
			after, err := os.ReadFile(f.store.positionPath())
			if err != nil || string(before) != string(after) {
				t.Fatalf("stale CAS changed position: %v", err)
			}
		})
	}
}

func TestWorkingPositionCASConcurrentSelectorsHaveOneWinner(t *testing.T) {
	f := newPositionCASFixture(t)
	cas, ok := any(f.store).(positionCAS)
	if !ok {
		t.Fatal("FileStore lacks atomic position/ref comparison")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		next := f.next
		e := *next.Selection
		e.ID = id
		next.Selection = &e
		go func() {
			<-start
			results <- cas.CompareAndSwapWorkingPosition(context.Background(), f.expected, next, f.ref)
		}()
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, domain.ErrSyncConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}
