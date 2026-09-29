package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestCheckoutJournalReplaysOriginalWorktreeBeforeAnotherWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte("checkout journal")))
	author := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "feature", strings.Repeat("a", 40))
	peer := NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "main", strings.Repeat("b", 40))
	id, err := author.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "transition"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := author.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*FileStore{author, peer} {
		if err := store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: store.gitBranch, GitCommit: store.gitCommit, Snapshot: id}); err != nil {
			t.Fatal(err)
		}
	}
	previousPeer, err := peer.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: id, BranchID: "feature-birth"}
	event, err := domain.NewBranchLifecycleRef(repo, "feature", id, 1, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	op := checkoutJournal{Version: 1, Transition: outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "feature"}, Branch: &branch, CreateBranch: true}, Lifecycle: &event, Position: &domain.WorkingPosition{RepoID: repo, WorktreeID: author.worktreeID, Branch: "feature", BranchID: "feature-birth", Snapshot: id, SharedTarget: id, GitCommit: author.gitCommit}}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(author.checkoutJournalPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// The next writer belongs to another worktree and must finish this journal
	// before proceeding, without adopting the author's selection itself.
	if err := peer.PutRef(ctx, domain.Ref{Kind: domain.RefTag, Name: "after-recovery", RepoID: repo, Target: id}); err != nil {
		t.Fatal(err)
	}
	got, err := author.ReadWorkingPosition(ctx, repo)
	if err != nil || got.BranchID != "feature-birth" || got.GitCommit != author.gitCommit {
		t.Fatalf("author not recovered=%+v %v", got, err)
	}
	gotPeer, err := peer.ReadWorkingPosition(ctx, repo)
	if err != nil || gotPeer.BranchID != previousPeer.BranchID || gotPeer.GitCommit != previousPeer.GitCommit {
		t.Fatalf("peer moved=%+v %v", gotPeer, err)
	}
	if _, err := os.Stat(author.checkoutJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed journal remains=%v", err)
	}
}

func TestCheckoutAcceptanceWaitsForSnapshotAndRetentionLocks(t *testing.T) {
	for _, lock := range []string{"snapshot", "collection"} {
		t.Run(lock, func(t *testing.T) {
			ctx := context.Background()
			store := NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte("checkout lock")))
			id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadCheckoutState(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			branch := domain.Ref{Kind: domain.RefBranch, Name: "prepared", RepoID: repo, Target: id}
			change := outbound.CheckoutTransition{RepoID: repo, Expected: before, Head: domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: branch.Name}, Branch: &branch, CreateBranch: true}
			blocked := func() error {
				limited, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				if err := store.CommitCheckout(limited, change); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("checkout bypassed %s lock: %v", lock, err)
				}
				return nil
			}
			if lock == "snapshot" {
				err = store.withSnapshotMutationLock(ctx, id, blocked)
			} else {
				var acquired bool
				acquired, err = store.TryCollectObjects(ctx, blocked)
				if !acquired {
					t.Fatal("collection lock not acquired")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.ReadCheckoutState(ctx, repo)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("blocked checkout mutated selection: %+v, %v", after, err)
			}
			if _, err := store.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("branch published while blocked: %v", err)
			}
			if _, err := os.Stat(store.checkoutJournalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal accepted while blocked: %v", err)
			}
			if err := store.CommitCheckout(ctx, change); err != nil {
				t.Fatalf("checkout failed after lock release: %v", err)
			}
		})
	}
}

func TestCheckoutRecoveryKeepsAcceptedMemoryAfterAttachmentChanges(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			store := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
			repo := string(domain.HashContent([]byte("accepted checkout memory")))
			id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
			if err != nil {
				t.Fatal(err)
			}
			old, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "accepted"})
			if err != nil {
				t.Fatal(err)
			}
			next, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "later", PreviousMemoryHash: old})
			if err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, MemoryHash: old}
			if err := store.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			p := domain.WorkingPosition{RepoID: repo, WorktreeID: store.worktreeID, BranchID: "position", LocalBranch: "main", GitCommit: store.gitCommit, Snapshot: id, MemoryHash: old, MemoryPinned: true, Rewound: true}
			change := outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Target: id}}
			if version == 2 {
				change.ExpectedMemoryHash, p.MemorySource = old, id
			}
			op := checkoutJournal{Version: version, Transition: change, Position: &p}
			raw, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.checkoutJournalPath(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			// The journal was already accepted. A later attachment writer does not
			// invalidate it or replace the prepared bytes during crash recovery.
			if err := store.CompareAndSwapSnapshotMemory(ctx, id, old, next); err != nil {
				t.Fatal(err)
			}
			peer := NewFileStore(root)
			if err := peer.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "recover", Target: id}); err != nil {
				t.Fatal(err)
			}
			got, err := store.ReadWorkingPosition(ctx, repo)
			if err != nil || got.MemoryHash != old || got.MemorySource != p.MemorySource || got.Snapshot != id || !got.MemoryPinned {
				t.Fatalf("accepted memory replaced: %+v; %v", got, err)
			}
			gotSnap, err := store.GetSnapshot(ctx, id)
			snap.MemoryHash = next
			if err != nil || !reflect.DeepEqual(gotSnap, snap) {
				t.Fatalf("replay changed snapshot: %+v; %v", gotSnap, err)
			}
			if _, err := os.Stat(store.checkoutJournalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains: %v", err)
			}
		})
	}
}

func TestCheckoutPinsReadOnlyAndIncludeHistoricalMemoryOwner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := NewFileStore(root)
	id, owner := domain.HashContent([]byte("target")), domain.HashContent([]byte("owner"))
	if pinned, err := store.HasCheckoutPin(ctx, id); err != nil || pinned {
		t.Fatalf("absent journal: %v %v", pinned, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read initialized storage: %v %v", entries, err)
	}
	repo := string(domain.HashContent([]byte("repo")))
	op := checkoutJournal{Version: 2, Transition: outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Target: id}, MemoryPin: &domain.AgentMemoryPin{SnapshotID: owner, MemoryHash: domain.HashContent([]byte("memory"))}}}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(store.checkoutJournalPath(), raw); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []domain.ContentHash{id, owner} {
		if pinned, err := store.HasCheckoutPin(ctx, hash); err != nil || !pinned {
			t.Fatalf("missing journal root %s: %v %v", hash, pinned, err)
		}
	}
	if pinned, err := store.HasCheckoutPin(ctx, domain.HashContent([]byte("unrelated"))); err != nil || pinned {
		t.Fatalf("unrelated pin: %v %v", pinned, err)
	}
	unchanged, err := os.ReadFile(store.checkoutJournalPath())
	if err != nil || string(unchanged) != string(raw) {
		t.Fatalf("pin lookup replayed journal: %v", err)
	}
	if _, err := store.GetRef(ctx, repo, domain.RefHEAD, "HEAD"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pin lookup published HEAD: %v", err)
	}
}

func TestCheckoutJournalRejectsUnsafeWorktreeIdentityBeforePublishing(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("unsafe journal")))
	id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "unsafe", RepoID: repo, Target: id}
	op := checkoutJournal{Version: 1, Transition: outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "unsafe"}, Branch: &branch, CreateBranch: true}, Position: &domain.WorkingPosition{RepoID: repo, WorktreeID: strings.Repeat("../", 10) + "xx", Snapshot: id}}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.checkoutJournalPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefTag, Name: "trigger", RepoID: repo, Target: id}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("unsafe journal accepted=%v", err)
	}
	if _, err := store.GetRef(ctx, repo, domain.RefBranch, "unsafe"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unsafe journal published branch=%v", err)
	}
}

func TestCheckoutRejectsIndexChangedDuringPreparation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte("checkout index fence")))
	store := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: store.gitCommit, Snapshot: id}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadCheckoutState(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	index, position, err := store.ReadStaging(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	next := index
	next.Sequence++
	next = next.WithRevision()
	if err := store.CompareAndSwapStaging(ctx, index.Revision, next, position); err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "prepared", RepoID: repo, Target: id}
	transition := outbound.CheckoutTransition{RepoID: repo, Expected: before, Head: domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: branch.Name}, Branch: &branch, CreateBranch: true}
	if err := store.CommitCheckout(ctx, transition); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("index change ignored: %v", err)
	}
	if _, err := store.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("index conflict published branch: %v", err)
	}
}
