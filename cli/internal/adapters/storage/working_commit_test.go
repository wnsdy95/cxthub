package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestWorkingCommitRecoveryKeepsAuthorCursorAndRetainedSource(t *testing.T) {
	for _, refWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-ref", true: "after-ref"}[refWritten], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			owner := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
			peer := NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "main", strings.Repeat("a", 40))
			repo := string(domain.HashContent([]byte("repo")))
			var ids []domain.ContentHash
			for _, label := range []string{"future", "continuation"} {
				id, err := owner.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{GitBranch: label}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := owner.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[0]}
			if _, err := owner.CreateBranchRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			if err := peer.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "main"}); err != nil {
				t.Fatal(err)
			}
			e := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "main", Branch: "main", Kind: "advance", Source: ids[0], Target: ids[1], CreatedAt: time.Now().UTC()}
			ref.Target = ids[1]
			op := workingCommit{Ref: ref, Expected: ids[0], Position: domain.WorkingPosition{RepoID: repo, Branch: "main", Snapshot: ids[1], SharedTarget: ids[1], WorktreeID: owner.worktreeID}, Event: &e}
			raw, _ := json.Marshal(op)
			if err := writeAtomic(owner.workingCommitPath(), raw); err != nil {
				t.Fatal(err)
			}
			if refWritten {
				if err := owner.putHistoryEvent(e); err != nil {
					t.Fatal(err)
				}
				if err := owner.putRefRaw(ref); err != nil {
					t.Fatal(err)
				}
			}
			// A different worktree is the first process after the interrupted save.
			p, err := peer.GetWorkingPosition(ctx)
			if err != nil || p.Snapshot != ids[0] {
				t.Fatalf("peer moved during recovery: %+v %v", p, err)
			}
			p, err = owner.GetWorkingPosition(ctx)
			if err != nil || p.Snapshot != ids[1] {
				t.Fatalf("owner not recovered: %+v %v", p, err)
			}
			retained, err := peer.GetRef(ctx, repo, domain.RefTag, "cxt/history/v1/"+e.ID+"/source")
			if err != nil || retained.Target != ids[0] {
				t.Fatalf("source not retained: %+v %v", retained, err)
			}
			if _, err := os.Stat(owner.workingCommitPath()); !os.IsNotExist(err) {
				t.Fatalf("journal not acknowledged: %v", err)
			}
			events, err := peer.ListHistoryEvents(ctx, repo)
			advances := 0
			for _, event := range events {
				if event.Kind == "advance" {
					advances++
				}
			}
			if err != nil || advances != 1 || len(events) != 3 {
				t.Fatalf("duplicate recovery event: %+v %v", events, err)
			}
		})
	}
}

func TestBranchRenamePreservesEachWorktreePositionAndMemory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "old", strings.Repeat("a", 40))
	b := NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "old", strings.Repeat("b", 40))
	repo := string(domain.HashContent([]byte("rename positions")))
	var before []domain.WorkingPosition
	for i, owner := range []*FileStore{a, b} {
		id, err := owner.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: owner.worktreeID}}})
		if err != nil {
			t.Fatal(err)
		}
		memory, err := owner.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: owner.worktreeID})
		if err != nil {
			t.Fatal(err)
		}
		if err := owner.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, MemoryHash: memory}); err != nil {
			t.Fatal(err)
		}
		p := domain.WorkingPosition{RepoID: repo, Branch: "old", BranchID: "one-task", Snapshot: id, MemoryHash: memory, MemorySource: id, MemoryPinned: true, GitCommit: owner.gitCommit, Rewound: i == 0}
		if err := owner.PutWorkingPosition(ctx, p); err != nil {
			t.Fatal(err)
		}
		before = append(before, p)
	}
	for n := 0; n < 2; n++ {
		if err := a.RenameWorkingBranch(ctx, repo, "old", "new"); err != nil {
			t.Fatal(err)
		}
	}
	for i, owner := range []*FileStore{a, b} {
		got, err := owner.GetWorkingPosition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := before[i]
		if got.Branch != "new" || got.BranchID != want.BranchID || got.Snapshot != want.Snapshot || got.MemoryHash != want.MemoryHash || got.GitCommit != want.GitCommit || got.Rewound != want.Rewound {
			t.Fatalf("position moved: %+v want %+v", got, want)
		}
	}
}

func TestBootstrapPositionAcceptsOnlyNewRepositoryObjectsAfterFirstPull(t *testing.T) {
	for _, oldEvidence := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-pull", true: "previous-repository"}[oldEvidence], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
			oldRepo, newRepo := string(domain.HashContent([]byte("before remote"))), string(domain.HashContent([]byte("server identity")))
			if err := s.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: oldRepo, Symbolic: "main"}); err != nil {
				t.Fatal(err)
			}
			id, err := s.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
			if err != nil {
				t.Fatal(err)
			}
			owner := newRepo
			if oldEvidence {
				owner = oldRepo
			}
			if err := s.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: owner}); err != nil {
				t.Fatal(err)
			}
			_, err = s.GetRef(ctx, newRepo, domain.RefHEAD, "HEAD")
			if oldEvidence && err != domain.ErrHashMismatch {
				t.Fatalf("previous identity evidence ignored: %v", err)
			}
			if !oldEvidence && err != nil {
				t.Fatalf("first pull falsely marked corrupt: %v", err)
			}
		})
	}
}
