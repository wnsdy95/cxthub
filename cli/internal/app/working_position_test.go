package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestOrphanInheritsAncestorMemoryWithVerifiedProvenance(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("repo")))
	var ids []domain.ContentHash
	for _, label := range []string{"project memory", "new dialogue"} {
		cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(strings.ReplaceAll(e2eClaudeSession, "hello", label)))
		if err != nil {
			t.Fatal(err)
		}
		id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
		if len(ids) == 0 {
			snap.MemoryHash, err = store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "project rules"})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			snap.Parents = []domain.ContentHash{ids[0]}
		}
		if err := store.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	svc := NewContextHistoryService(store, store)
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), BranchID: "orphan", RepoID: repo, Branch: "fresh", Kind: "orphan", MemorySource: ids[1], CreatedAt: time.Now().UTC()}
	got, err := svc.ValidateHistorySource(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "" || got.Target != "" || got.MemorySource != ids[0] || got.MemoryHash == "" || !got.MemoryPinned {
		t.Fatalf("orphan memory/ancestry: %+v", got)
	}
	// A recorded empty historical memory must not be replaced by an ancestor's
	// newer mutable attachment. Unknown and explicitly empty are distinct.
	e.MemoryPinned = true
	pinned, err := svc.ValidateHistorySource(ctx, e)
	if err != nil || pinned.MemoryHash != "" {
		t.Fatalf("empty memory repinned: %+v %v", pinned, err)
	}
	// A valid hash from an unrelated snapshot is still invalid provenance.
	got.MemorySource = ids[1]
	if _, err := svc.ValidateHistorySource(ctx, got); err != domain.ErrHashMismatch {
		t.Fatalf("foreign digest accepted: %v", err)
	}
}

func TestSaveUsesSelectedPastAndRetainsFutureWithoutGraft(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "rewind"
		if orphan {
			name = "orphan"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			root := t.TempDir()
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
				b, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s %v", args, b, err)
				}
				return strings.TrimSpace(string(b))
			}
			run("init", "-q", "-b", "main")
			run("config", "core.hooksPath", "/dev/null")
			run("-c", "user.name=test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
			commit := run("rev-parse", "HEAD")
			store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", commit)
			peer := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "main", commit)
			repo, err := gitctx.NewGitContextAdapter().CurrentRepo(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			add := func(label string, parent domain.ContentHash) domain.ContentHash {
				cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(strings.ReplaceAll(e2eClaudeSession, "hello", label)))
				if err != nil {
					t.Fatal(err)
				}
				id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
				if err != nil {
					t.Fatal(err)
				}
				snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: "main"}
				if parent != "" {
					snap.Parents = []domain.ContentHash{parent}
				}
				if err := store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
				return id
			}
			a := add("past", "")
			b := add("future", a)
			if _, err := store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: b}); err != nil {
				t.Fatal(err)
			}
			if err := peer.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: "main"}); err != nil {
				t.Fatal(err)
			}
			p := domain.WorkingPosition{RepoID: repo.ID, Branch: "main", Snapshot: a, GitCommit: commit}
			if orphan {
				p.Snapshot = ""
				p.MemorySource = a
				p.Orphan = true
			}
			h := NewContextHistoryService(store, store)
			if err := h.SelectPosition(ctx, p); err != nil {
				t.Fatal(err)
			}
			session := filepath.Join(root, "session.jsonl")
			if err := os.WriteFile(session, []byte(strings.ReplaceAll(e2eClaudeSession, "hello", "new work")), 0600); err != nil {
				t.Fatal(err)
			}
			svc := newTestSaveService(gitctx.NewGitContextAdapter(), map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}, store)
			out, err := svc.Save(ctx, inbound.SaveInput{Cwd: root, Provider: domain.ProviderClaude, SessionPath: session, Branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			d, err := store.GetSnapshot(ctx, out.SnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.GraftParents) != 0 {
				t.Fatalf("future reattached: %+v", d)
			}
			if orphan {
				if len(d.Parents) != 0 {
					t.Fatalf("orphan ancestry: %+v", d)
				}
			} else if len(d.Parents) != 1 || d.Parents[0] != a {
				t.Fatalf("selected source lost: %+v", d)
			}
			events, err := store.ListHistoryEvents(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			var advance *domain.HistoryEvent
			for _, e := range events {
				if e.Kind == "advance" {
					e := e
					advance = &e
				}
			}
			if advance == nil || advance.Source != b || advance.Target != d.ID {
				t.Fatalf("missing retention transition: %+v", events)
			}
			retained, err := store.GetRef(ctx, repo.ID, domain.RefTag, "cxt/history/v1/"+advance.ID+"/source")
			if err != nil || retained.Target != b {
				t.Fatalf("retention: %+v %v", retained, err)
			}
			peerHead, err := peer.GetRef(ctx, repo.ID, domain.RefHEAD, "HEAD")
			if err != nil || peerHead.Target != b {
				t.Fatalf("peer cursor moved: %+v %v", peerHead, err)
			}
			own, err := store.GetRef(ctx, repo.ID, domain.RefHEAD, "HEAD")
			if err != nil || own.Target != d.ID {
				t.Fatalf("own cursor: %+v %v", own, err)
			}
			again, err := svc.Save(ctx, inbound.SaveInput{Cwd: root, Provider: domain.ProviderClaude, SessionPath: session, Branch: "main"})
			if err != nil || again.SnapshotID != d.ID {
				t.Fatalf("retry: %+v %v", again, err)
			}
			after, err := store.ListHistoryEvents(ctx, repo.ID)
			if err != nil || len(after) != len(events) {
				t.Fatalf("duplicate history: %d -> %d %v", len(events), len(after), err)
			}
		})
	}
}

func TestForwardCodeSelectionIsNotARewind(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	repo := string(domain.HashContent([]byte("repo")))
	var ids []domain.ContentHash
	for _, text := range []string{"before", "after"} {
		cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(strings.ReplaceAll(e2eClaudeSession, "hello", text)))
		if err != nil {
			t.Fatal(err)
		}
		id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
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
	if _, err := store.CreateBranchRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: ids[0]}); err != nil {
		t.Fatal(err)
	}
	h := NewContextHistoryService(store, store)
	if err := h.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", Snapshot: ids[1], GitCommit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	p, err := h.CurrentPosition(ctx)
	if err != nil || p.Rewound {
		t.Fatalf("fast-forward selected as rewind: %+v %v", p, err)
	}
}

func TestHistoricalSelectionPinsMemoryAcrossLaterMetadataUpdates(t *testing.T) {
	for _, initiallyEmpty := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved-memory", true: "no-memory"}[initiallyEmpty], func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
			repo := string(domain.HashContent([]byte("repo")))
			id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
			if err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
			if !initiallyEmpty {
				snap.MemoryHash, err = store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "memory at selected time"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := store.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			h := NewContextHistoryService(store, store)
			// Detached is an explicitly historical selection even at a shared tip.
			if err := h.SelectPosition(ctx, domain.WorkingPosition{RepoID: repo, Snapshot: id, GitCommit: strings.Repeat("a", 40)}); err != nil {
				t.Fatal(err)
			}
			newer, err := store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "future native memory"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CompareAndSwapSnapshotMemory(ctx, id, snap.MemoryHash, newer); err != nil {
				t.Fatal(err)
			}
			snap.MemoryHash = newer
			for _, projection := range []func(context.Context, MemoryReader, domain.Snapshot) (domain.MemoryDigest, bool, bool){snapshotMemoryProjectionDetailed, priorMemoryProjectionDetailed} {
				got, found, complete := projection(ctx, store, snap)
				if !complete || found == initiallyEmpty || strings.Contains(got.Summary, "future") {
					t.Fatalf("historical memory contaminated: %+v found=%v complete=%v", got, found, complete)
				}
				if !initiallyEmpty && got.Summary != "memory at selected time" {
					t.Fatalf("selected version lost: %+v", got)
				}
			}
		})
	}
}
