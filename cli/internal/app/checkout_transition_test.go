package app

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

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type checkoutPrepareFunc func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error)

func (f checkoutPrepareFunc) Load(ctx context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
	return f(ctx, in)
}

func TestCheckoutPrepareFailureDoesNotCreateBranch(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	before, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewCheckoutSessionService(NewForkSessionService(store), branchLifecycleFailLoad{}, store)
	if _, err := svc.Checkout(ctx, inbound.CheckoutInput{From: "main", NewBranch: "prepared-only"}); err == nil {
		t.Fatal("provider failure succeeded")
	}
	if _, err := store.GetRef(ctx, "", domain.RefBranch, "prepared-only"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed preparation published branch: %v", err)
	}
	after, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || after != before {
		t.Fatalf("failed preparation changed HEAD: %+v %v", after, err)
	}
}

func TestCheckoutDoesNotOverwriteConcurrentSelection(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	target := seedClaudeSnapshot(t, store)
	concurrent := domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", Target: target}
	prepare := checkoutPrepareFunc(func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
		return inbound.LoadOutput{}, store.PutRef(ctx, concurrent)
	})
	svc := NewCheckoutSessionService(NewForkSessionService(store), prepare, store)
	if _, err := svc.Checkout(ctx, inbound.CheckoutInput{From: "main", NewBranch: "racing"}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("race = %v", err)
	}
	if _, err := store.GetRef(ctx, "", domain.RefBranch, "racing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("race created branch: %v", err)
	}
	after, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || after != concurrent {
		t.Fatalf("concurrent selection overwritten: %+v %v", after, err)
	}
}

func TestCheckoutDetachedPositionIsWorktreeLocal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	code := strings.Repeat("a", 40)
	first := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", code)
	second := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "second"), "main", code)
	repo := string(domain.HashContent([]byte("detached checkout")))
	var ids []domain.ContentHash
	for _, text := range []string{"selected", "other worktree"} {
		doc := pullDoc(t, text)
		if _, err := first.PutDoc(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := first.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[1]}
	if err := first.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*storage.FileStore{first, second} {
		if err := store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: code, Snapshot: ids[1], SharedTarget: ids[1]}); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewCheckoutSessionService(NewForkSessionService(first), nil, first)
	out, err := svc.Checkout(ctx, inbound.CheckoutInput{RepoID: repo, From: string(ids[0]), SkipMaterialize: true})
	if err != nil {
		t.Fatal(err)
	}
	head, err := first.GetRef(ctx, repo, domain.RefHEAD, "HEAD")
	if err != nil || head.Target != out.Head || head.Symbolic != "" {
		t.Fatalf("returned/actual head mismatch: %+v %+v %v", out, head, err)
	}
	position, err := first.GetWorkingPosition(ctx)
	if err != nil || position.Branch != "" || position.Snapshot != ids[0] {
		t.Fatalf("not detached: %+v %v", position, err)
	}
	peer, err := second.GetWorkingPosition(ctx)
	if err != nil || peer.Snapshot != ids[1] {
		t.Fatalf("peer moved: %+v %v", peer, err)
	}
	shared, err := first.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || shared.Target != ids[1] {
		t.Fatalf("shared branch moved: %+v %v", shared, err)
	}
}

func TestManualCheckoutRefusesUnverifiedCodeBeforePreparation(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	called := false
	prepare := checkoutPrepareFunc(func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
		called = true
		return inbound.LoadOutput{}, nil
	})
	svc := NewCheckoutSessionService(NewForkSessionService(store), prepare, store)
	_, err := svc.CheckoutAtCode(ctx, inbound.CheckoutInput{From: "main", NewBranch: "wrong-code"}, strings.Repeat("f", 40))
	if err == nil || !strings.Contains(err.Error(), "code_position_mismatch") || called {
		t.Fatalf("mismatch error=%v prepared=%v", err, called)
	}
	if _, err := store.GetRef(ctx, "", domain.RefBranch, "wrong-code"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("mismatch published ref: %v", err)
	}
}

func TestReferenceLoadDoesNotMoveWorkingHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	reference := seedClaudeSnapshot(t, store)
	otherDoc := pullDoc(t, "other current work")
	if _, err := store.PutDoc(ctx, otherDoc); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: otherDoc.Hash, DocHash: otherDoc.Hash, Branch: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "other", Target: otherDoc.Hash}); err != nil {
		t.Fatal(err)
	}
	expected := domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "other"}
	if err := store.PutRef(ctx, expected); err != nil {
		t.Fatal(err)
	}
	if _, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: string(reference), TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || after != expected {
		t.Fatalf("reference-only load moved HEAD: %+v %v", after, err)
	}
}

type changedCodeBindingStore struct {
	*storage.FileStore
	afterRead func() error
}

func (s *changedCodeBindingStore) ListHistoryEvents(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	events, err := s.FileStore.ListHistoryEvents(ctx, repo)
	if err == nil && s.afterRead != nil {
		fn := s.afterRead
		s.afterRead = nil
		err = fn()
	}
	return events, err
}

func TestCodeCheckedCheckoutDoesNotFollowBranchChangedAfterValidation(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("code fence repo")))
	code := strings.Repeat("a", 40)
	store := storage.NewFileStore(t.TempDir())
	var ids []domain.ContentHash
	for _, text := range []string{"checked code", "later code"} {
		doc := pullDoc(t, text)
		if _, err := store.PutDoc(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := store.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[0]}
	if err := store.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "main"}); err != nil {
		t.Fatal(err)
	}
	event := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "main", Branch: "main", Kind: "position", Source: ids[0], Target: ids[0], GitAfter: code, CreatedAt: time.Now().UTC()}
	if err := store.PutHistoryEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	wrapped := &changedCodeBindingStore{FileStore: store, afterRead: func() error { ref.Target = ids[1]; return store.PutRef(ctx, ref) }}
	prepared := false
	load := checkoutPrepareFunc(func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
		prepared = true
		return inbound.LoadOutput{}, nil
	})
	svc := NewCheckoutSessionService(NewForkSessionService(wrapped), load, wrapped)
	_, err := svc.CheckoutAtCode(ctx, inbound.CheckoutInput{RepoID: repo, From: "main", NewBranch: "checked"}, code)
	if !errors.Is(err, domain.ErrSyncConflict) || prepared {
		t.Fatalf("stale code check accepted: err=%v prepared=%v", err, prepared)
	}
	if _, err := store.GetRef(ctx, repo, domain.RefBranch, "checked"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale checked ref published: %v", err)
	}
}

func TestCheckoutGitMoveDuringPreparationDoesNotPublishSelection(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, st)
	before, err := st.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	git := &stagingGit{sha: strings.Repeat("a", 40)}
	prepare := checkoutPrepareFunc(func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
		git.sha = strings.Repeat("b", 40)
		return inbound.LoadOutput{}, nil
	})
	svc := NewCheckoutSessionService(NewForkSessionService(st), prepare, st).WithCodePosition(git)
	if _, err = svc.Checkout(ctx, inbound.CheckoutInput{From: "main", NewBranch: "stale-code"}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("Git moved: %v", err)
	}
	after, _ := st.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if after != before {
		t.Fatal("moved HEAD for stale code")
	}
	if _, err = st.GetRef(ctx, "", domain.RefBranch, "stale-code"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("published stale-code branch")
	}
}

func TestCheckoutFencesPreparedMemoryIncludingEmptyAttachment(t *testing.T) {
	for _, change := range []string{"unchanged", "unchanged-empty", "replace", "attach", "clear"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			doc := pullDoc(t, "prepared checkout source")
			if _, err := f.store.PutDoc(ctx, doc); err != nil {
				t.Fatal(err)
			}
			old, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "prepared memory"})
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "new attachment", PreviousMemoryHash: old})
			if err != nil {
				t.Fatal(err)
			}
			initial := old
			if change == "attach" || change == "unchanged-empty" {
				initial = ""
			}
			snap := domain.Snapshot{RepoID: f.git.repo.ID, ID: doc.Hash, DocHash: doc.Hash, MemoryHash: initial}
			if err := f.store.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			prepared := false
			load := checkoutPrepareFunc(func(context.Context, inbound.LoadInput) (inbound.LoadOutput, error) {
				observed, err := f.store.GetSnapshot(ctx, doc.Hash)
				if err != nil || observed.MemoryHash != initial {
					t.Fatalf("wrong prepared attachment: %+v; %v", observed, err)
				}
				prepared = true
				switch change {
				case "replace", "attach":
					err = f.store.CompareAndSwapSnapshotMemory(ctx, doc.Hash, initial, next)
				case "clear":
					err = f.store.CompareAndSwapSnapshotMemory(ctx, doc.Hash, initial, "")
				}
				return inbound.LoadOutput{}, err
			})
			svc := NewCheckoutSessionService(NewForkSessionService(f.store), load, f.store).WithCodePosition(f.git)
			_, err = svc.Checkout(ctx, inbound.CheckoutInput{RepoID: f.git.repo.ID, From: string(doc.Hash), NewBranch: "prepared", Cwd: f.root})
			if !prepared {
				t.Fatal("provider preparation was not exercised")
			}
			if change == "unchanged" || change == "unchanged-empty" {
				if err != nil {
					t.Fatal(err)
				}
				p, err := f.store.GetWorkingPosition(ctx)
				if err != nil || p.Snapshot != doc.Hash || p.MemoryHash != initial || !p.MemoryPinned || p.Rewound {
					t.Fatalf("prepared memory not pinned: %+v; %v", p, err)
				}
			} else {
				if !errors.Is(err, domain.ErrSelectionChanged) {
					t.Fatalf("changed prepared memory was accepted: %v", err)
				}
				after, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("memory conflict changed selection/index: %+v; %v", after, err)
				}
				if _, err := f.store.GetRef(ctx, f.git.repo.ID, domain.RefBranch, "prepared"); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("memory conflict created a branch: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(f.root, ".cxt", "checkout-transition.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unfinished journal remains: %v", err)
			}
			observed, err := f.store.GetSnapshot(ctx, doc.Hash)
			if err != nil || observed.ID != snap.ID || observed.DocHash != snap.DocHash || !reflect.DeepEqual(observed.Parents, snap.Parents) {
				t.Fatalf("checkout rewrote source identity/parents: %+v; %v", observed, err)
			}
		})
	}
}

func TestCheckoutPreservesHistoricalPreparedMemoryAndParents(t *testing.T) {
	for _, kind := range []string{"same-owner", "ancestor", "empty", "attachment-race"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			parent, target := pullDoc(t, "historical parent"), pullDoc(t, "historical target")
			for _, doc := range []domain.SessionDoc{parent, target} {
				if _, err := f.store.PutDoc(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.PutSnapshot(ctx, domain.Snapshot{ID: parent.Hash, DocHash: parent.Hash, RepoID: f.git.repo.ID}); err != nil {
				t.Fatal(err)
			}
			owner := target.Hash
			if kind == "ancestor" {
				owner = parent.Hash
			}
			old, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner, Summary: "historical"})
			if err != nil {
				t.Fatal(err)
			}
			latest, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target.Hash, Summary: "current attachment"})
			if err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: target.Hash, DocHash: target.Hash, RepoID: f.git.repo.ID, Parents: []domain.ContentHash{parent.Hash}, MemoryHash: latest}
			if err := f.store.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			pin := domain.AgentMemoryPin{SnapshotID: owner, MemoryHash: old}
			if kind == "empty" {
				pin = domain.AgentMemoryPin{}
			}
			p := domain.WorkingPosition{RepoID: f.git.repo.ID, Branch: "main", GitCommit: f.git.sha, Snapshot: target.Hash, MemoryHash: pin.MemoryHash, MemorySource: pin.SnapshotID, MemoryPinned: true, Rewound: true}
			if err := f.store.PutWorkingPosition(ctx, p); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			load := checkoutPrepareFunc(func(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
				if in.Ref != string(target.Hash) || in.Branch != "" || in.MemoryPin == nil || *in.MemoryPin != pin {
					t.Fatalf("wrong historical preparation: %+v", in)
				}
				if kind == "attachment-race" {
					return inbound.LoadOutput{}, f.store.CompareAndSwapSnapshotMemory(ctx, target.Hash, latest, "")
				}
				return inbound.LoadOutput{}, nil
			})
			_, err = NewCheckoutSessionService(nil, load, f.store).Checkout(ctx, inbound.CheckoutInput{RepoID: f.git.repo.ID, From: string(target.Hash), NewBranch: "restored"})
			if kind == "attachment-race" {
				if !errors.Is(err, domain.ErrSelectionChanged) {
					t.Fatalf("historical pin bypassed attachment CAS: %v", err)
				}
				after, err := f.store.ReadCheckoutState(ctx, f.git.repo.ID)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("race changed selection: %+v %v", after, err)
				}
				snap.MemoryHash = ""
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got, err := f.store.GetWorkingPosition(ctx)
				if err != nil || got.Snapshot != target.Hash || got.MemoryHash != pin.MemoryHash || got.MemorySource != pin.SnapshotID || !got.MemoryPinned {
					t.Fatalf("historical pin lost: %+v %v", got, err)
				}
				if !got.Rewound {
					t.Fatal("named historical checkout re-enabled live memory attachment updates")
				}
				// Selecting the newly named branch again must keep the same historical
				// memory, including an explicit empty pin or an ancestor-owned digest.
				repeatLoad := checkoutPrepareFunc(func(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
					if in.Branch != "restored" || in.MemoryPin == nil || *in.MemoryPin != pin {
						t.Fatalf("repeated named checkout lost historical pin: %+v", in)
					}
					return inbound.LoadOutput{}, nil
				})
				if _, err := NewCheckoutSessionService(nil, repeatLoad, f.store).Checkout(ctx, inbound.CheckoutInput{RepoID: f.git.repo.ID, From: "restored"}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.store.GetSnapshot(ctx, target.Hash)
			if err != nil || !reflect.DeepEqual(got, snap) {
				t.Fatalf("checkout rewrote original snapshot/parents: %+v %v", got, err)
			}
		})
	}
}

func TestCheckoutPreparationKeepsSourceBranchSeparateFromDestination(t *testing.T) {
	for _, source := range []string{"branch", "HEAD", "tag", "hash"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			store := storage.NewFileStore(t.TempDir())
			id := seedClaudeSnapshot(t, store)
			if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "source", Target: id}); err != nil {
				t.Fatal(err)
			}
			if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefTag, Name: "saved", Target: id}); err != nil {
				t.Fatal(err)
			}
			from, want := "source", "source"
			switch source {
			case "HEAD":
				from, want = "HEAD", "main"
			case "tag":
				from, want = "saved", ""
			case "hash":
				from, want = string(id), ""
			}
			load := checkoutPrepareFunc(func(_ context.Context, in inbound.LoadInput) (inbound.LoadOutput, error) {
				if in.Ref != string(id) || in.Branch != want {
					t.Fatalf("source replaced with destination/current branch: %+v, want %q", in, want)
				}
				return inbound.LoadOutput{}, nil
			})
			if _, err := NewCheckoutSessionService(nil, load, store).Checkout(ctx, inbound.CheckoutInput{From: from, NewBranch: "destination"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckoutJournalRetainsHookCaptureUntilRecovery(t *testing.T) {
	for _, journalState := range []string{"accepted", "malformed", "future-version", "invalid-head", "invalid-position"} {
		t.Run(journalState, func(t *testing.T) {
			ctx := context.Background()
			f := newStagingFixture(t)
			repo := f.git.repo.ID
			put := func(texts ...string) domain.Snapshot {
				t.Helper()
				doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "journal-session", Fidelity: domain.FidelityFull}}}
				for i, text := range texts {
					doc.CIR.Events = append(doc.CIR.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
				}
				id, err := f.store.PutDoc(ctx, doc)
				if err != nil {
					t.Fatal(err)
				}
				snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Provider: domain.ProviderCodex, SessionID: "journal-session", Message: domain.HookMessagePrefix + " capture"}
				if err := f.store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
				return snap
			}
			old, next := put("first"), put("first", "second")
			_, p, err := f.store.ReadStaging(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			p.Branch, p.BranchID, p.LocalBranch = "", "position", "main"
			p.Snapshot, p.MemoryPinned, p.Rewound = old.ID, true, true
			journal := struct {
				Version    int                         `json:"version"`
				Transition outbound.CheckoutTransition `json:"transition"`
				Position   *domain.WorkingPosition     `json:"position"`
			}{1, outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Target: old.ID}}, &p}
			switch journalState {
			case "future-version":
				journal.Version = 99
			case "invalid-head":
				journal.Transition.Head.Target = "invalid"
			case "invalid-position":
				journal.Position.WorktreeID = "../invalid"
			}
			raw, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if journalState == "malformed" {
				raw = []byte("interrupted/corrupt journal")
			}
			path := filepath.Join(f.root, ".cxt", "checkout-transition.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Model a crash after durable acceptance but before any ref or position
			// exists. A restarted collector has no in-process retention lease.
			peer := storage.NewFileStore(f.root)
			collector := newTestSaveService(nil, nil, nil, peer)
			collector.gcHookLeaf(ctx, repo, old.ID, next.ID)
			got, err := peer.GetSnapshot(ctx, old.ID)
			if err != nil || !reflect.DeepEqual(got, old) {
				t.Fatalf("journal-owned source was deleted or rewritten: %+v; %v", got, err)
			}
			if _, err := peer.GetDoc(ctx, old.DocHash); err != nil {
				t.Fatalf("journal-owned document deleted: %v", err)
			}
			jobs, err := peer.CaptureCollections(ctx, repo, 32)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("collection obligation lost: %+v; %v", jobs, err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != string(raw) {
				t.Fatalf("GC recovered or changed the journal: %v", err)
			}
			if journalState != "accepted" {
				return
			}
			if err := peer.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "after-recovery", Target: next.ID}); err != nil {
				t.Fatalf("next ref writer could not recover checkout: %v", err)
			}
			position, err := f.store.ReadWorkingPosition(ctx, repo)
			if err != nil || position.Snapshot != old.ID || position.WorktreeID != p.WorktreeID {
				t.Fatalf("recovery lost the original selection: %+v; %v", position, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed journal remains: %v", err)
			}
			collector.gcHookLeaf(ctx, repo, old.ID, next.ID)
			jobs, err = peer.CaptureCollections(ctx, repo, 32)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("completed journal leaked a collection retry: %+v; %v", jobs, err)
			}
			if _, err := peer.GetDoc(ctx, old.ID); err != nil {
				t.Fatalf("recovered selection no longer retains its source: %v", err)
			}
		})
	}
}
