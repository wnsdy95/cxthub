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

type captureEvidenceFixture struct {
	store     *FileStore
	repo      string
	base      domain.ContentHash
	next      domain.ContentHash
	selection domain.HistoryEvent
	position  domain.WorkingPosition
}

func newCaptureEvidenceFixture(t *testing.T) captureEvidenceFixture {
	t.Helper()
	root := t.TempDir()
	f := captureEvidenceFixture{
		store: NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40)),
		repo:  string(domain.HashContent([]byte("passive capture evidence"))),
	}
	for i, target := range []*domain.ContentHash{&f.base, &f.next} {
		hash, err := f.store.PutDoc(context.Background(), domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: fmt.Sprintf("capture-evidence-%d", i)}}})
		if err != nil {
			t.Fatal(err)
		}
		*target = hash
		if err := f.store.PutSnapshot(context.Background(), domain.Snapshot{ID: hash, DocHash: hash, RepoID: f.repo, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	f.selection = domain.HistoryEvent{
		ID: strings.Repeat("1", 32), RepoID: f.repo, Branch: "main", BranchID: domain.LegacyContextBranchID(f.repo, "main"),
		Kind: "position", Source: f.base, Target: f.base, WorktreeID: f.store.worktreeID,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	f.position = domain.WorkingPosition{
		RepoID: f.repo, WorktreeID: f.store.worktreeID, Branch: "main", BranchID: f.selection.BranchID,
		Snapshot: f.base, SharedTarget: f.base, GitCommit: f.store.gitCommit, Selection: &f.selection,
	}
	captureEvidenceWriteJSON(t, f.store.positionPath(), f.position)
	if err := f.store.putRefRaw(domain.Ref{Kind: domain.RefBranch, RepoID: f.repo, Name: "main", BranchID: f.selection.BranchID, Target: f.base}); err != nil {
		t.Fatal(err)
	}
	captureEvidenceWrite(t, filepath.Join(f.store.storeDir(), "HEAD"), []byte("ref: refs/heads/main\n"))
	// Fixture writers create locks. Remove them before the observation so a
	// reader that accidentally acquires a mutation lock cannot hide that write.
	if err := os.RemoveAll(filepath.Join(f.store.storeDir(), "locks")); err != nil {
		t.Fatal(err)
	}
	return f
}

func captureEvidenceWrite(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := writeAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}

func captureEvidenceWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	captureEvidenceWrite(t, path, raw)
}

func captureEvidenceState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var raw []byte
		if !entry.IsDir() {
			raw, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state[path] = fmt.Sprintf("%v/%d/%s", info.Mode(), info.ModTime().UnixNano(), raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertCaptureEvidenceUnchanged(t *testing.T, f captureEvidenceFixture, before map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, captureEvidenceState(t, f.store.repoRoot)) {
		t.Fatal("capture evidence read changed files, metadata, directories or pending journals")
	}
}

func (f captureEvidenceFixture) pendingWorkingCommit(t *testing.T) {
	t.Helper()
	event := f.selection
	event.ID, event.Kind, event.Target = strings.Repeat("2", 32), "advance", f.next
	position := f.position
	position.Snapshot, position.SharedTarget, position.Selection = f.next, f.next, nil
	op := workingCommit{
		Expected: f.base, Position: position, Event: &event,
		Ref: domain.Ref{Kind: domain.RefBranch, RepoID: f.repo, Name: "main", BranchID: position.BranchID, Target: f.next},
	}
	if err := validateWorkingCommit(op); err != nil {
		t.Fatalf("pending working commit must be valid: %v", err)
	}
	captureEvidenceWriteJSON(t, f.store.workingCommitPath(), op)
}

func (f captureEvidenceFixture) pendingCheckout(t *testing.T) {
	t.Helper()
	branch := domain.Ref{Kind: domain.RefBranch, RepoID: f.repo, Name: "feature", BranchID: "feature-birth", Target: f.next}
	position := domain.WorkingPosition{
		RepoID: f.repo, WorktreeID: f.store.worktreeID, Branch: branch.Name, BranchID: branch.BranchID,
		Snapshot: f.next, SharedTarget: f.next, GitCommit: f.store.gitCommit, MemoryPinned: true,
	}
	op := checkoutJournal{
		Version: 2, Position: &position,
		Transition: outbound.CheckoutTransition{
			RepoID: f.repo, CreateBranch: true, Branch: &branch,
			Head: domain.Ref{Kind: domain.RefHEAD, RepoID: f.repo, Name: "HEAD", Symbolic: branch.Name},
		},
	}
	if target, err := checkoutJournalTarget(op); err != nil || target != f.next {
		t.Fatalf("pending checkout must be valid: %s, %v", target, err)
	}
	captureEvidenceWriteJSON(t, f.store.checkoutJournalPath(), op)
}

func TestCaptureEvidenceReaderPendingTransactionsStayUnchanged(t *testing.T) {
	for _, kind := range []string{"working", "checkout", "both", "corrupt working", "corrupt checkout", "both corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := newCaptureEvidenceFixture(t)
			switch kind {
			case "working":
				f.pendingWorkingCommit(t)
			case "checkout":
				f.pendingCheckout(t)
			case "both":
				f.pendingWorkingCommit(t)
				f.pendingCheckout(t)
			case "corrupt working":
				captureEvidenceWrite(t, f.store.workingCommitPath(), []byte("corrupt pending working commit"))
			case "corrupt checkout":
				captureEvidenceWrite(t, f.store.checkoutJournalPath(), []byte("corrupt pending checkout"))
			case "both corrupt":
				captureEvidenceWrite(t, f.store.workingCommitPath(), []byte("{}"))
				captureEvidenceWrite(t, f.store.checkoutJournalPath(), []byte("{}"))
			}
			before := captureEvidenceState(t, f.store.repoRoot)
			reader := NewCaptureEvidenceReader(f.store.repoRoot)
			for _, hash := range []domain.ContentHash{f.base, f.next} {
				snapshot, err := reader.GetSnapshot(context.Background(), hash)
				if err != nil || snapshot.ID != hash || snapshot.DocHash != hash || snapshot.RepoID != f.repo {
					t.Fatalf("snapshot evidence: %+v, %v", snapshot, err)
				}
			}
			events, err := reader.ListHistoryEvents(context.Background(), f.repo)
			if err != nil || !reflect.DeepEqual(events, []domain.HistoryEvent{f.selection}) {
				t.Fatalf("pending journal altered passive position evidence: %+v, %v", events, err)
			}
			assertCaptureEvidenceUnchanged(t, f, before)
		})
	}
}

func TestCaptureEvidenceReaderKeepsPositionsScopeAndDependencies(t *testing.T) {
	f := newCaptureEvidenceFixture(t)
	parent := f.selection
	parent.ID, parent.Kind = strings.Repeat("3", 32), "birth"
	parent.CreatedAt = parent.CreatedAt.Add(time.Hour)
	f.selection.BindingParent = parent.ID
	f.position.Selection = &f.selection
	captureEvidenceWriteJSON(t, f.store.positionPath(), f.position)
	captureEvidenceWriteJSON(t, filepath.Join(f.store.storeDir(), "history", parent.ID+".json"), parent)
	other := parent
	other.ID, other.RepoID = strings.Repeat("4", 32), string(domain.HashContent([]byte("other capture repo")))
	captureEvidenceWriteJSON(t, filepath.Join(f.store.storeDir(), "history", other.ID+".json"), other)
	before := captureEvidenceState(t, f.store.repoRoot)
	events, err := NewCaptureEvidenceReader(f.store.repoRoot).ListHistoryEvents(context.Background(), f.repo)
	// The current position selection exists only in position.json. Its later
	// timestamped dependency must still precede it, and other repos stay out.
	if err != nil || !reflect.DeepEqual(events, []domain.HistoryEvent{parent, f.selection}) {
		t.Fatalf("scope, position or causal order changed: %+v, %v", events, err)
	}
	assertCaptureEvidenceUnchanged(t, f, before)
}

func TestCaptureEvidenceReaderRejectsInvalidHistoryEvidence(t *testing.T) {
	for _, kind := range []string{"conflicting persisted selection", "conflicting position selections", "position path", "invalid event", "missing dependency"} {
		t.Run(kind, func(t *testing.T) {
			f := newCaptureEvidenceFixture(t)
			wantHashMismatch := false
			switch kind {
			case "conflicting persisted selection":
				conflict := f.selection
				conflict.Target = f.next
				captureEvidenceWriteJSON(t, filepath.Join(f.store.storeDir(), "history", conflict.ID+".json"), conflict)
				wantHashMismatch = true
			case "conflicting position selections":
				other := f.position
				other.WorktreeID = strings.Repeat("c", 32)
				conflict := f.selection
				conflict.Target = f.next
				other.Selection = &conflict
				captureEvidenceWriteJSON(t, filepath.Join(f.store.storeDir(), "worktrees", other.WorktreeID, "position.json"), other)
				wantHashMismatch = true
			case "position path":
				f.position.WorktreeID = strings.Repeat("c", 32)
				captureEvidenceWriteJSON(t, f.store.positionPath(), f.position)
				wantHashMismatch = true
			case "invalid event":
				f.selection.Target = "invalid hash"
				f.position.Selection = &f.selection
				captureEvidenceWriteJSON(t, f.store.positionPath(), f.position)
			case "missing dependency":
				f.selection.BindingParent = strings.Repeat("d", 32)
				f.position.Selection = &f.selection
				captureEvidenceWriteJSON(t, f.store.positionPath(), f.position)
			}
			before := captureEvidenceState(t, f.store.repoRoot)
			events, err := NewCaptureEvidenceReader(f.store.repoRoot).ListHistoryEvents(context.Background(), f.repo)
			if err == nil || events != nil || (wantHashMismatch && !errors.Is(err, domain.ErrHashMismatch)) {
				t.Fatalf("invalid evidence accepted or typed error lost: %+v, %v", events, err)
			}
			if kind == "missing dependency" && !strings.Contains(err.Error(), "missing history dependency") {
				t.Fatalf("dependency validation changed: %v", err)
			}
			assertCaptureEvidenceUnchanged(t, f, before)
		})
	}
}

func TestCaptureEvidenceReaderDeduplicatesIdenticalSelection(t *testing.T) {
	f := newCaptureEvidenceFixture(t)
	captureEvidenceWriteJSON(t, filepath.Join(f.store.storeDir(), "history", f.selection.ID+".json"), f.selection)
	before := captureEvidenceState(t, f.store.repoRoot)
	events, err := NewCaptureEvidenceReader(f.store.repoRoot).ListHistoryEvents(context.Background(), f.repo)
	if err != nil || !reflect.DeepEqual(events, []domain.HistoryEvent{f.selection}) {
		t.Fatalf("identical current position event duplicated: %+v, %v", events, err)
	}
	assertCaptureEvidenceUnchanged(t, f, before)
}

func TestCaptureEvidenceReaderSnapshotValidation(t *testing.T) {
	f := newCaptureEvidenceFixture(t)
	reader := NewCaptureEvidenceReader(f.store.repoRoot)
	missing := domain.HashContent([]byte("missing passive snapshot"))
	if _, err := reader.GetSnapshot(context.Background(), missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing snapshot type changed: %v", err)
	}
	captureEvidenceWriteJSON(t, f.store.objectPath("snapshots", f.base), domain.Snapshot{ID: f.next, DocHash: f.next, RepoID: f.repo})
	before := captureEvidenceState(t, f.store.repoRoot)
	if _, err := reader.GetSnapshot(context.Background(), f.base); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("snapshot identity validation weakened: %v", err)
	}
	assertCaptureEvidenceUnchanged(t, f, before)
}

// Deterministic cancellation checks both sides of the delegated read without
// racing a goroutine or relying on a timer. The wrapped context owns Done.
type captureEvidenceCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *captureEvidenceCancelContext) Err() error {
	if ctx.remaining > 0 {
		ctx.remaining--
		if ctx.remaining == 0 {
			ctx.cancel()
		}
	}
	return ctx.Context.Err()
}

func TestCaptureEvidenceReaderCancellation(t *testing.T) {
	for _, kind := range []string{"history before", "history after", "snapshot before", "snapshot after"} {
		t.Run(kind, func(t *testing.T) {
			f := newCaptureEvidenceFixture(t)
			f.pendingWorkingCommit(t)
			// Corrupt evidence makes the post-read test verify that typed context
			// cancellation takes precedence over a returned validation error.
			captureEvidenceWrite(t, f.store.positionPath(), []byte("broken position"))
			captureEvidenceWrite(t, f.store.objectPath("snapshots", f.base), []byte("broken snapshot"))
			before := captureEvidenceState(t, f.store.repoRoot)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &captureEvidenceCancelContext{Context: base, cancel: cancel}
			if strings.HasSuffix(kind, "before") {
				cancel()
			} else if strings.HasPrefix(kind, "history") {
				ctx.remaining = 2 // adapter entry and post-history checks
			} else {
				ctx.remaining = 3 // adapter entry, snapshot entry and adapter post-read
			}
			reader := NewCaptureEvidenceReader(f.store.repoRoot)
			var err error
			if strings.HasPrefix(kind, "history") {
				var events []domain.HistoryEvent
				events, err = reader.ListHistoryEvents(ctx, f.repo)
				if events != nil {
					t.Fatalf("canceled read returned evidence: %+v", events)
				}
			} else {
				var snapshot domain.Snapshot
				snapshot, err = reader.GetSnapshot(ctx, f.base)
				if !reflect.DeepEqual(snapshot, domain.Snapshot{}) {
					t.Fatalf("canceled read returned snapshot: %+v", snapshot)
				}
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("typed cancellation lost: %v", err)
			}
			assertCaptureEvidenceUnchanged(t, f, before)
		})
	}
}

func TestCaptureEvidenceReaderMissingReplicaStaysAbsent(t *testing.T) {
	root := t.TempDir()
	before := captureEvidenceState(t, root)
	reader := NewCaptureEvidenceReader(root)
	events, err := reader.ListHistoryEvents(context.Background(), "")
	if err != nil || len(events) != 0 {
		t.Fatalf("missing replica history: %+v, %v", events, err)
	}
	if _, err := reader.GetSnapshot(context.Background(), domain.HashContent([]byte("missing"))); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing replica snapshot: %v", err)
	}
	if !reflect.DeepEqual(before, captureEvidenceState(t, root)) {
		t.Fatal("passive evidence reader created replica directories")
	}
}
