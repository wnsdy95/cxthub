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
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type trackingFixture struct {
	store, peer         *FileStore
	commit              outbound.TrackingAttachmentCommit
	peerBefore          domain.WorkingPosition
	old, chosen, shared domain.ContentHash
}

func trackingJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}

func trackingSnapshot(t *testing.T, s *FileStore, repo, label string, parents ...domain.ContentHash) domain.ContentHash {
	t.Helper()
	id, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: sampleCIR(label)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(context.Background(), domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "team-task", Parents: parents}); err != nil {
		t.Fatal(err)
	}
	return id
}

func newTrackingFixture(t *testing.T) trackingFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	code := strings.Repeat("a", 40)
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "local-task", code)
	peer := NewWorktreeFileStore(root, filepath.Join(root, ".git", "worktrees", "peer"), "main", code)
	repo := string(domain.HashContent([]byte("tracking fixture repository")))
	f := trackingFixture{store: s, peer: peer}
	f.old = trackingSnapshot(t, s, repo, "old working selection")
	f.chosen = trackingSnapshot(t, s, repo, "chosen historical context")
	f.shared = trackingSnapshot(t, s, repo, "newer server tip", f.chosen)
	memory, err := s.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.chosen, Summary: "recorded pin"})
	if err != nil {
		t.Fatal(err)
	}
	mutable, err := s.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.chosen, PreviousMemoryHash: memory, Summary: "newer mutable memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapSnapshotMemory(ctx, f.chosen, "", mutable); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []*FileStore{s, peer} {
		if err := owner.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", BranchID: domain.LegacyContextBranchID(repo, "main"), GitCommit: code, Snapshot: f.old, MemoryPinned: true}); err != nil {
			t.Fatal(err)
		}
	}
	old, err := s.readPosition()
	if err != nil {
		t.Fatal(err)
	}
	f.peerBefore, err = peer.readPosition()
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "team-task", BranchID: "remote-task", Kind: "birth", Source: f.chosen, Target: f.chosen, MemoryHash: memory, MemoryPinned: true, GitAfter: code, CreatedAt: created}
	observation := birth
	observation.ID, observation.Kind, observation.CreatedAt = strings.Repeat("2", 32), "advance", created.Add(time.Second)
	e := observation
	e.ID, e.Kind, e.LocalBranch, e.WorktreeID = strings.Repeat("3", 32), "attach", "local-task", s.worktreeID
	e.SharedTarget, e.CreatedAt = f.shared, created.Add(2*time.Second)
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: f.shared}
	selection := e
	selection.ID, selection.Kind, selection.Source, selection.GitBefore = strings.Repeat("4", 32), "position", old.Snapshot, old.GitCommit
	next := domain.WorkingPosition{RepoID: repo, WorktreeID: s.worktreeID, Branch: e.Branch, BranchID: e.BranchID, LocalBranch: e.LocalBranch, GitCommit: code, Snapshot: f.chosen, SharedTarget: f.shared, MemoryHash: memory, MemoryPinned: true, Rewound: true, Selection: &selection}
	f.commit = outbound.TrackingAttachmentCommit{Attachment: domain.TrackingAttachment{Event: e, ObservedRef: ref, Proof: []domain.HistoryEvent{birth, observation}, Code: code, Rewound: true}, Position: &outbound.TrackingPositionCAS{Expected: &old, Next: next}}
	return f
}

func (f trackingFixture) journal(t *testing.T) trackingAttachmentJournal {
	t.Helper()
	hash, err := trackingAttachmentHash(f.commit.Attachment)
	if err != nil {
		t.Fatal(err)
	}
	var j trackingAttachmentJournal
	err = f.store.withRefMutationLock(context.Background(), func() error {
		var err error
		j, err = f.store.prepareTrackingAttachment(context.Background(), f.commit, hash)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// Simulate crash prefixes directly, following existing working-commit tests.
// No provider, native Git command, child process or timing-dependent crash.
func (f trackingFixture) prefix(t *testing.T, j trackingAttachmentJournal, step int) {
	t.Helper()
	s, a := f.store, j.Commit.Attachment
	trackingJSON(t, s.trackingAttachmentPath(), j)
	if step >= 1 {
		for _, e := range a.Proof {
			if err := s.putHistoryEvent(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if step >= 2 {
		if err := s.putHistoryEvent(a.Event); err != nil {
			t.Fatal(err)
		}
	}
	if step >= 3 && j.Lifecycle != nil {
		if err := s.putRefRaw(*j.Lifecycle); err != nil {
			t.Fatal(err)
		}
	}
	if step >= 4 {
		if err := s.putRefRaw(trackingRef(a)); err != nil {
			t.Fatal(err)
		}
	}
	if step >= 5 {
		if err := s.writeLocalBinding(trackingBinding(a)); err != nil {
			t.Fatal(err)
		}
	}
	if step >= 6 && j.Commit.Position != nil {
		if err := s.writePosition(j.Commit.Position.Next); err != nil {
			t.Fatal(err)
		}
	}
	if step >= 7 {
		trackingJSON(t, s.trackingReceiptPath(a), trackingAttachmentReceipt{1, a.Event.RepoID, a.Event.ID, j.ProofHash})
	}
}

func trackingMutableFiles(t *testing.T, s *FileStore) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, area := range []string{"history", "refs", "branch-bindings", "worktrees", "HEAD", "tracking-attachments"} {
		err := filepath.WalkDir(filepath.Join(s.storeDir(), area), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(raw)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func (f trackingFixture) applied(t *testing.T) {
	t.Helper()
	a := f.commit.Attachment
	ref, err := f.store.GetRef(context.Background(), a.Event.RepoID, domain.RefBranch, a.Event.Branch)
	if err != nil || ref != trackingRef(a) {
		t.Fatalf("ref identity/shared target: %v", err)
	}
	binding, err := f.store.ResolveLocalBranch(context.Background(), a.Event.RepoID, a.Event.LocalBranch)
	if err != nil || !binding.Tracking || binding.BranchID != a.Event.BranchID {
		t.Fatalf("durable binding: %v", err)
	}
	if f.commit.Position != nil {
		p, err := f.store.readPosition()
		if err != nil || !reflect.DeepEqual(p, f.commit.Position.Next) {
			t.Fatalf("frozen selection not restored: %v", err)
		}
	}
	peer, err := f.peer.readPosition()
	if err != nil || !reflect.DeepEqual(peer, f.peerBefore) {
		t.Fatalf("peer selection changed: %v", err)
	}
	if _, err := os.Stat(f.store.trackingAttachmentPath()); !os.IsNotExist(err) {
		t.Fatal("redo still pending")
	}
	hash, err := trackingAttachmentHash(a)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := f.store.trackingAttachmentApplied(a, hash); err != nil || !done {
		t.Fatalf("missing completion: %v", err)
	}
}

func TestTrackingAttachmentSuccessAndReceipt(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned-empty=%v", empty), func(t *testing.T) {
			f := newTrackingFixture(t)
			if empty {
				f.commit.Attachment.Event.MemoryHash = ""
				for i := range f.commit.Attachment.Proof {
					f.commit.Attachment.Proof[i].MemoryHash = ""
				}
				f.commit.Position.Next.MemoryHash, f.commit.Position.Next.Selection.MemoryHash = "", ""
			}
			before, _ := f.store.GetSnapshot(context.Background(), f.chosen)
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
				t.Fatal(err)
			}
			f.applied(t)
			after, _ := f.store.GetSnapshot(context.Background(), f.chosen)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("mutable snapshot adopted")
			}
			// Another accepted writer moves both ref and position before Git ack.
			ref := trackingRef(f.commit.Attachment)
			ref.Target = f.old
			if err := f.store.PutRef(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			p := f.commit.Position.Next
			p.Snapshot, p.MemoryHash, p.Selection = f.old, "", nil
			if err := f.store.PutWorkingPosition(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			files := trackingMutableFiles(t, f.store)
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(files, trackingMutableFiles(t, f.store)) {
				t.Fatal("ack retry overwrote later work")
			}
			f.commit.Attachment.Event.CreatedAt = f.commit.Attachment.Event.CreatedAt.Add(time.Second)
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("same ID changed payload: %v", err)
			}
		})
	}
}

func TestTrackingAttachmentRecoversEveryPrefixBeforePeerWriter(t *testing.T) {
	for step := 0; step <= 7; step++ {
		t.Run(fmt.Sprintf("prefix-%d", step), func(t *testing.T) {
			f := newTrackingFixture(t)
			// Code is an ancestor, so replayed selection is not remote evidence.
			f.commit.Attachment.Code = strings.Repeat("b", 40)
			for i := range f.commit.Attachment.Proof {
				f.commit.Attachment.Proof[i].GitAfter = f.commit.Attachment.Code
			}
			j := f.journal(t)
			f.prefix(t, j, step)
			if err := f.peer.PutRef(context.Background(), domain.Ref{RepoID: f.commit.Attachment.Event.RepoID, Kind: domain.RefTag, Name: "after-tracking", Target: f.old}); err != nil {
				t.Fatal(err)
			}
			f.applied(t)
			events, err := f.store.ListHistoryEvents(context.Background(), f.commit.Attachment.Event.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, e := range events {
				if e.ID == f.commit.Attachment.Event.ID {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("attach occurrences=%d", count)
			}
		})
	}
}

func TestTrackingAttachmentRejectsBeforeAcceptance(t *testing.T) {
	cases := map[string]func(*testing.T, *trackingFixture){
		"new-ref": func(t *testing.T, f *trackingFixture) {
			if _, err := f.store.CreateBranchRef(context.Background(), trackingRef(f.commit.Attachment)); err != nil {
				t.Fatal(err)
			}
		},
		"local-birth": func(t *testing.T, f *trackingFixture) {
			e := f.commit.Attachment.Proof[0]
			e.ID, e.BranchID = strings.Repeat("5", 32), "local-unpublished"
			if err := f.store.PutHistoryEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		},
		"unpublished-pin": func(t *testing.T, f *trackingFixture) {
			e := f.commit.Attachment.Proof[1]
			e.ID, e.Kind, e.MemoryHash = strings.Repeat("5", 32), "position", ""
			if err := f.store.PutHistoryEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		},
		"binding": func(t *testing.T, f *trackingFixture) {
			e := f.commit.Attachment.Event
			e.ID, e.BranchID, e.Branch = strings.Repeat("5", 32), "another-task", "another"
			if err := f.store.BindLocalBranch(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		},
		"position": func(t *testing.T, f *trackingFixture) {
			p := *f.commit.Position.Expected
			p.MemoryPinned = !p.MemoryPinned
			trackingJSON(t, f.store.positionPath(), p)
		},
		"document": func(t *testing.T, f *trackingFixture) {
			if err := os.WriteFile(f.store.objectPath("docs", f.chosen), []byte("damaged"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"memory": func(t *testing.T, f *trackingFixture) {
			if err := os.WriteFile(f.store.objectPath("memories", f.commit.Attachment.Event.MemoryHash), []byte("damaged"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong-owner":        func(t *testing.T, f *trackingFixture) { f.commit.Position.Next.WorktreeID = strings.Repeat("f", 32) },
		"wrong-pin":          func(t *testing.T, f *trackingFixture) { f.commit.Position.Next.MemoryHash = "" },
		"wrong-ref-identity": func(t *testing.T, f *trackingFixture) { f.commit.Attachment.ObservedRef.BranchID = "wrong" },
		"selection-id-collision": func(t *testing.T, f *trackingFixture) {
			f.commit.Position.Next.Selection.ID = f.commit.Attachment.Event.ID
		},
		"binding-only-proof-id-collision": func(t *testing.T, f *trackingFixture) {
			f.commit.Position = nil
			f.commit.Attachment.Event.ID = f.commit.Attachment.Proof[0].ID
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTrackingFixture(t)
			change(t, &f)
			before := trackingMutableFiles(t, f.store)
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err == nil {
				t.Fatal("invalid attachment accepted")
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("rejected attachment changed applied state")
			}
			if _, err := os.Stat(f.store.trackingAttachmentPath()); !os.IsNotExist(err) {
				t.Fatal("rejected attachment accepted redo")
			}
		})
	}
}

func TestTrackingAttachmentPinsAndInspectionArePassive(t *testing.T) {
	f := newTrackingFixture(t)
	repo := f.commit.Attachment.Event.RepoID
	owner := trackingSnapshot(t, f.store, repo, "historical memory owner")
	memory, err := f.store.PutMemory(context.Background(), domain.MemoryDigest{SnapshotID: owner, Summary: "inherited pin"})
	if err != nil {
		t.Fatal(err)
	}
	f.commit.Attachment.Event.MemorySource, f.commit.Attachment.Event.MemoryHash = owner, memory
	f.commit.Attachment.Proof[1].MemorySource, f.commit.Attachment.Proof[1].MemoryHash = owner, memory
	f.commit.Position.Next.MemorySource, f.commit.Position.Next.MemoryHash = owner, memory
	f.commit.Position.Next.Selection.MemorySource, f.commit.Position.Next.Selection.MemoryHash = owner, memory
	j := f.journal(t)
	f.prefix(t, j, 0)
	before := trackingMutableFiles(t, f.store)
	for _, root := range []domain.ContentHash{f.old, f.chosen, f.shared, owner} {
		if pinned, err := f.store.HasTrackingAttachmentPin(context.Background(), root); err != nil || !pinned {
			t.Fatalf("missing root pin: %v", err)
		}
	}
	report := f.store.InspectReplica(context.Background())
	if !strings.Contains(strings.Join(report.Issues, " "), "pending local transaction: tracking-attachment.json") {
		t.Fatal("inspection omitted pending operation")
	}
	if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
		t.Fatal("passive read replayed operation")
	}
	j.Version = 42
	trackingJSON(t, f.store.trackingAttachmentPath(), j)
	if _, err := f.store.HasTrackingAttachmentPin(context.Background(), f.old); err == nil {
		t.Fatal("corrupt journal allowed GC")
	}
	if report := f.store.InspectReplica(context.Background()); !strings.Contains(strings.Join(report.Issues, " "), "tracking-attachment.json:") {
		t.Fatal("inspection omitted corrupt journal")
	}
}

func TestTrackingAttachmentRecoveryBlocksOtherWritersOnChangedEvidence(t *testing.T) {
	for _, fault := range []string{"ref", "binding", "position", "history", "document", "journal"} {
		t.Run(fault, func(t *testing.T) {
			f := newTrackingFixture(t)
			j := f.journal(t)
			f.prefix(t, j, 4)
			switch fault {
			case "ref":
				ref := trackingRef(f.commit.Attachment)
				ref.Target = f.old
				if err := f.store.putRefRaw(ref); err != nil {
					t.Fatal(err)
				}
			case "binding":
				record := trackingBinding(f.commit.Attachment)
				record.Detached = true
				if err := f.store.writeLocalBinding(record); err != nil {
					t.Fatal(err)
				}
			case "position":
				p := *f.commit.Position.Expected
				p.MemoryPinned = false
				trackingJSON(t, f.store.positionPath(), p)
			case "history":
				e := f.commit.Attachment.Proof[1]
				e.ID, e.MemoryHash = strings.Repeat("5", 32), ""
				if err := f.store.putHistoryEvent(e); err != nil {
					t.Fatal(err)
				}
			case "document":
				if err := os.WriteFile(f.store.objectPath("docs", f.chosen), []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "journal":
				j.Commit.Position.Next.WorktreeID = "../escape"
				trackingJSON(t, f.store.trackingAttachmentPath(), j)
			}
			before := trackingMutableFiles(t, f.store)
			err := f.peer.PutRef(context.Background(), domain.Ref{RepoID: f.commit.Attachment.Event.RepoID, Kind: domain.RefTag, Name: "must-not-publish", Target: f.old})
			if err == nil {
				t.Fatal("writer passed unresolved recovery")
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("failed recovery performed further writes")
			}
			if _, err := os.Stat(f.store.trackingAttachmentPath()); err != nil {
				t.Fatal("pending journal lost")
			}
		})
	}
}

func TestTrackingAttachmentConcurrentAttemptsHaveOneWinner(t *testing.T) {
	f := newTrackingFixture(t)
	other := f.commit
	other.Attachment.Event.ID = strings.Repeat("6", 32)
	p, selection := *f.commit.Position, *f.commit.Position.Next.Selection
	selection.ID = strings.Repeat("7", 32)
	p.Next.Selection, other.Position = &selection, &p
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, c := range []outbound.TrackingAttachmentCommit{f.commit, other} {
		go func(c outbound.TrackingAttachmentCommit) {
			<-start
			results <- f.store.CommitTrackingAttachment(context.Background(), c)
		}(c)
	}
	close(start)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, domain.ErrSyncConflict) {
			conflicts++
		} else {
			t.Errorf("unexpected result: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestTrackingAttachmentNormalWriterWinsBeforeAcceptance(t *testing.T) {
	f := newTrackingFixture(t)
	held, release := make(chan struct{}), make(chan struct{})
	writer, apply := make(chan error, 1), make(chan error, 1)
	go func() {
		writer <- f.store.withRefMutationLock(context.Background(), func() error { close(held); <-release; return f.store.putRefRaw(trackingRef(f.commit.Attachment)) })
	}()
	<-held
	go func() { apply <- f.peer.CommitTrackingAttachment(context.Background(), f.commit) }()
	close(release)
	writerErr, applyErr := <-writer, <-apply
	if writerErr != nil || !errors.Is(applyErr, domain.ErrSyncConflict) {
		t.Fatalf("writer=%v apply=%v", writerErr, applyErr)
	}
	if _, err := os.Stat(f.store.trackingReceiptPath(f.commit.Attachment)); !os.IsNotExist(err) {
		t.Fatal("losing attachment marked complete")
	}
}

func TestTrackingAttachmentCancellationDoesNotAccept(t *testing.T) {
	f := newTrackingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := trackingMutableFiles(t, f.store)
	if err := f.store.CommitTrackingAttachment(ctx, f.commit); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
		t.Fatal("cancelled attachment changed state")
	}
}

func TestTrackingAttachmentWithoutPositionAndLegacyIdentity(t *testing.T) {
	f := newTrackingFixture(t)
	before, _ := f.store.readPosition()
	f.commit.Position = nil
	identity := domain.LegacyContextBranchID(f.commit.Attachment.Event.RepoID, f.commit.Attachment.Event.Branch)
	f.commit.Attachment.ObservedRef.BranchID = ""
	f.commit.Attachment.Event.BranchID = identity
	f.commit.Attachment.Proof = f.commit.Attachment.Proof[1:]
	f.commit.Attachment.Proof[0].BranchID = identity
	if err := f.peer.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
		t.Fatal(err)
	}
	f.applied(t)
	after, _ := f.store.readPosition()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("binding-only apply moved owner")
	}
}

func TestTrackingAttachmentExistingRefCASAndArchive(t *testing.T) {
	for _, scenario := range []string{"forward", "divergent", "identity", "archived", "stale-full-ref"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTrackingFixture(t)
			ref := trackingRef(f.commit.Attachment)
			ref.Target = f.chosen
			if scenario == "divergent" {
				ref.Target = f.old
			}
			if scenario == "identity" {
				ref.Target, ref.BranchID = f.shared, "another-incarnation"
			}
			if _, err := f.store.CreateBranchRef(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			f.commit.ExpectedRef = &ref
			if scenario == "archived" {
				if _, err := f.store.ArchiveBranchRef(context.Background(), ref.RepoID, ref.Name); err != nil {
					t.Fatal(err)
				}
				f.commit.ExpectedRef = nil
			}
			if scenario == "stale-full-ref" {
				changed := ref
				changed.BranchID = "another-incarnation"
				if err := f.store.PutRef(context.Background(), changed); err != nil {
					t.Fatal(err)
				}
			}
			before := trackingMutableFiles(t, f.store)
			err := f.store.CommitTrackingAttachment(context.Background(), f.commit)
			if scenario == "forward" {
				if err != nil {
					t.Fatal(err)
				}
				f.applied(t)
			} else {
				if err == nil {
					t.Fatal("conflicting ref accepted")
				}
				if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
					t.Fatal("ref rejection changed state")
				}
			}
		})
	}
}

func TestTrackingAttachmentRecoveryExcludesOnlyExactOwnSelection(t *testing.T) {
	for _, differentID := range []bool{false, true} {
		t.Run(fmt.Sprintf("different-id=%v", differentID), func(t *testing.T) {
			f := newTrackingFixture(t)
			f.commit.Attachment.Code = strings.Repeat("b", 40)
			for i := range f.commit.Attachment.Proof {
				f.commit.Attachment.Proof[i].GitAfter = f.commit.Attachment.Code
			}
			f.prefix(t, f.journal(t), 6)
			e := *f.commit.Position.Next.Selection
			if differentID {
				e.ID = strings.Repeat("5", 32)
			} else {
				e.MemoryHash = ""
			}
			// Simulate a competing ordinary observation, without invoking recovery.
			trackingJSON(t, filepath.Join(f.store.storeDir(), "history", e.ID+".json"), e)
			before := trackingMutableFiles(t, f.store)
			if err := f.peer.PutRef(context.Background(), domain.Ref{RepoID: e.RepoID, Kind: domain.RefTag, Name: "blocked", Target: f.old}); err == nil {
				t.Fatal("unowned/changed selection was ignored")
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("failed recovery wrote state")
			}
		})
	}
}

func TestTrackingAttachmentRepairCannotBypassPendingJournal(t *testing.T) {
	f := newTrackingFixture(t)
	j := f.journal(t)
	j.Version = 42
	trackingJSON(t, f.store.trackingAttachmentPath(), j)
	source := NewFileStore(t.TempDir())
	repo := f.commit.Attachment.Event.RepoID
	trackingSnapshot(t, source, repo, "chosen historical context")
	before := trackingMutableFiles(t, f.store)
	_, err := f.store.RepairFromReplica(context.Background(), source, repo, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "tracking attachment still needs evidence") {
		t.Fatalf("repair guard: %v", err)
	}
	if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
		t.Fatal("repair bypassed accepted tracking operation")
	}
}
