package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func frozenFixture(t *testing.T) (stagingFixture, domain.CaptureAttempt) {
	t.Helper()
	f := newStagingFixture(t)
	index := f.stage(t, f.source(t, "base", "base context"))
	if _, err := f.svc.Commit(context.Background(), inbound.StagingCommitInput{Cwd: f.root, Message: "base", ExpectedRevision: index.Revision}); err != nil {
		t.Fatal(err)
	}
	position, err := f.store.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	memory, err := f.store.PutMemory(context.Background(), domain.MemoryDigest{SnapshotID: position.Snapshot, Summary: "ORIGINAL MEMORY"})
	if err != nil {
		t.Fatal(err)
	}
	position.MemoryHash, position.MemorySource, position.MemoryPinned = memory, position.Snapshot, true
	if err := f.store.PutWorkingPosition(context.Background(), position); err != nil {
		t.Fatal(err)
	}
	position, err = f.store.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := f.source(t, "frozen", "only the frozen message")
	input, err := f.svc.save.FreezeInput(context.Background(), f.root, source.Provider, source.Path)
	if err != nil {
		t.Fatal(err)
	}
	proof := domain.HistoryEvent{ID: strings.Repeat("4", 32), Kind: "position", RepoID: f.git.repo.ID, BranchID: position.BranchID, Branch: position.Branch, LocalBranch: position.LocalBranch, WorktreeID: position.WorktreeID, GitAfter: f.git.sha, MemoryPinned: true, MemoryHash: memory, MemorySource: position.Snapshot, CreatedAt: time.Now().UTC()}
	pass := domain.CaptureAttempt{Version: 2, Proof: proof, Initial: position.Snapshot, FrozenPosition: &position, InputsReady: true, Message: "original commit", Outcomes: []domain.CaptureOutcome{{Provider: source.Provider, State: "pending", Input: &input}}}
	if err := pass.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source.Path); err != nil {
		t.Fatal(err)
	}
	return f, pass
}

func completeFrozenTest(t *testing.T, f stagingFixture, p domain.CaptureAttempt, out inbound.SaveOutput) domain.CaptureAttempt {
	t.Helper()
	p.Outcomes[0].State, p.Outcomes[0].Target = "saved", out.SnapshotID
	p.Outcomes[0].MemoryHash, p.Outcomes[0].MemorySource = out.MemoryHash, out.MemorySource
	p.Proof.Source, p.Proof.Target = out.SnapshotID, out.SnapshotID
	events, err := f.store.ListHistoryEvents(context.Background(), f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if p.MatchesObservation(out.SnapshotID, e) {
			copy := e
			p.Observation = &copy
		}
	}
	if p.Observation == nil {
		t.Fatal("missing exact observation")
	}
	p.Complete, p.MemoryFinalized = true, true
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFrozenCaptureDeletedSourceRetryPinsOriginalMemory(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A newer memory attachment is not evidence for the older frozen commit.
	later, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Initial, Summary: "LATER MEMORY"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.GetSnapshot(ctx, p.Initial)
	if err != nil {
		t.Fatal(err)
	}
	snap.MemoryHash = later
	if err := f.store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil || out != again {
		t.Fatalf("replay: %v %v %v", out, again, err)
	}
	p = completeFrozenTest(t, f, p, out)
	if p.Observation.MemoryHash != p.Proof.MemoryHash {
		t.Fatal("imported later memory")
	}
	doc, err := f.store.GetDoc(ctx, out.SnapshotID)
	if err != nil || len(doc.CIR.Events) != 1 {
		t.Fatal("frozen input was lost", err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("projection moved worktree", err)
	}
	if err := f.svc.save.ApplyFrozen(ctx, f.root, p); err != nil {
		t.Fatal(err)
	}
	after, err = f.store.GetWorkingPosition(ctx)
	if err != nil || after.Snapshot != out.SnapshotID || after.MemoryHash != p.Proof.MemoryHash {
		t.Fatal("exact-position completion not applied", after, err)
	}
}

func TestFrozenCaptureCannotMoveChangedCodeSelection(t *testing.T) {
	for _, change := range []string{"code", "memory", "branch", "worktree"} {
		t.Run(change, func(t *testing.T) {
			f, p := frozenFixture(t)
			ctx := context.Background()
			out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
			if err != nil {
				t.Fatal(err)
			}
			p = completeFrozenTest(t, f, p, out)
			current, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "code":
				f.git.sha = strings.Repeat("b", 40)
			case "branch":
				f.git.branch = "other"
			case "memory":
				current.MemoryHash = ""
				current.MemorySource = ""
				if err := f.store.PutWorkingPosition(ctx, current); err != nil {
					t.Fatal(err)
				}
			case "worktree":
				p.FrozenPosition.WorktreeID = strings.Repeat("f", 32)
				p.Proof.WorktreeID = p.FrozenPosition.WorktreeID
				p.Observation.WorktreeID = p.FrozenPosition.WorktreeID
			}
			before, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.svc.save.ApplyFrozen(ctx, f.root, p); err != nil {
				t.Fatal(err)
			}
			after, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("changed selection overwritten", err)
			}
		})
	}
}

func TestFrozenCaptureRejectsUnsealedOrForeignAttempt(t *testing.T) {
	f, p := frozenFixture(t)
	p.InputsReady = false
	if _, err := f.svc.save.SaveFrozen(context.Background(), f.root, p, 0); err == nil {
		t.Fatal("unsealed input accepted")
	}
	p.InputsReady = true
	p.Proof.RepoID = string(domain.HashContent([]byte("other")))
	p.FrozenPosition.RepoID = p.Proof.RepoID
	if _, err := f.svc.save.SaveFrozen(context.Background(), f.root, p, 0); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal(err)
	}
}

type frozenFirstWriter struct {
	*storage.FileStore
	before func(domain.Snapshot) error
}

func (s *frozenFirstWriter) PutSnapshot(ctx context.Context, snap domain.Snapshot) error {
	if s.before != nil {
		fn := s.before
		s.before = nil
		if err := fn(snap); err != nil {
			return err
		}
	}
	return s.FileStore.PutSnapshot(ctx, snap)
}

func TestFrozenCaptureReconcilesConcurrentFirstWriter(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	competing := &frozenFirstWriter{FileStore: f.store}
	competing.before = func(snap domain.Snapshot) error {
		// A live writer wins the same document ID with different immutable parents.
		snap.Parents = nil
		return f.store.PutSnapshot(ctx, snap)
	}
	f.svc.save.store = competing
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Parents) != 0 {
		t.Fatal("winner natural parents overwritten")
	}
	ok, err := f.svc.save.frozenReachable(ctx, p.Proof.RepoID, out.SnapshotID, p.Initial)
	if err != nil || !ok {
		t.Fatal("frozen baseline lost", err)
	}
	_ = completeFrozenTest(t, f, p, out)
}

type frozenLocatedSource struct {
	outbound.CaptureSource
	path string
}

func (s frozenLocatedSource) LocateActiveSession(context.Context, string) (string, error) {
	return s.path, nil
}

func TestFrozenCaptureDiscoversUnmanagedSession(t *testing.T) {
	f := newStagingFixture(t)
	source := f.source(t, "unmanaged", "owned by ordinary Git")
	f.svc.save.captures[source.Provider] = frozenLocatedSource{f.svc.save.captures[source.Provider], source.Path}
	in, err := f.svc.save.FreezeInput(context.Background(), f.root, source.Provider, "")
	if err != nil || in.SourcePath != source.Path {
		t.Fatal("unmanaged provider source not frozen", err)
	}
}

func TestFrozenCaptureRejectsChangedOwnedSessionIdentity(t *testing.T) {
	f, p := frozenFixture(t)
	p.Outcomes[0].SessionID = "another-owned-session"
	if _, err := f.svc.save.SaveFrozen(context.Background(), f.root, p, 0); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("owned session identity ignored", err)
	}
}

type forbiddenFrozenMemory struct{}

func (forbiddenFrozenMemory) Provider() domain.ProviderKind { return domain.ProviderClaude }
func (forbiddenFrozenMemory) ReadNative(context.Context, string, string) (domain.NativeMemory, bool, error) {
	return domain.NativeMemory{}, false, errors.New("read future native memory")
}

func TestFrozenCaptureDerivesFreshMemoryWithoutFutureFilesOrAttachments(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	p.Outcomes[0].Input.NativeMemory = &domain.NativeMemory{Provider: domain.ProviderClaude, Source: "synthetic", Text: "FROZEN NATIVE BASELINE"}
	f.svc.save.WithFrozenMemory(map[domain.ProviderKind]outbound.MemorySource{domain.ProviderClaude: forbiddenFrozenMemory{}}, memory.NewRuleDistiller())
	plan, err := f.svc.save.PrepareFrozenMemory(ctx, f.root, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Outcomes[0].MemoryPlan = plan
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.GetMemory(ctx, out.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ORIGINAL MEMORY", "FROZEN NATIVE BASELINE", "only the frozen message"} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("lost %q: %s", want, got.Summary)
		}
	}
	if out.MemorySource != out.SnapshotID || got.SnapshotID != out.SnapshotID {
		t.Fatal("derived memory owner differs")
	}
	later, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: out.SnapshotID, PreviousMemoryHash: out.MemoryHash, Summary: "FUTURE MEMORY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, out.SnapshotID, out.MemoryHash, later); err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil || again != out {
		t.Fatal("historical memory replay changed", again, err)
	}
	snap, err := f.store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil || snap.MemoryHash != later {
		t.Fatal("later memory was overwritten", err)
	}
	complete := completeFrozenTest(t, f, p, out)
	if complete.Observation.MemoryHash != out.MemoryHash {
		t.Fatal("observation used later attachment")
	}
}

func TestFrozenCaptureDiagnosticsExcludePrivateNativeMemory(t *testing.T) {
	f, p := frozenFixture(t)
	p.Outcomes[0].Input.NativeMemory = &domain.NativeMemory{Provider: domain.ProviderClaude, Text: "private diagnostic sentinel"}
	journal := &recoveryFixture{attempts: []domain.CaptureAttempt{p}, resolutions: map[string]domain.CaptureResolution{}}
	states, err := NewCaptureRecoveryService(journal, f.store).Inspect(context.Background(), p.Proof.RepoID)
	if err != nil || len(states) != 1 {
		t.Fatal(err)
	}
	raw, err := json.Marshal(states)
	if err != nil || strings.Contains(string(raw), "private diagnostic sentinel") {
		t.Fatal("private native memory leaked to diagnostics", err)
	}
	if p.Outcomes[0].Input.NativeMemory == nil || states[0].Fingerprint != p.Fingerprint() {
		t.Fatal("diagnostics mutated frozen proof")
	}
}
