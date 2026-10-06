package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type workingReadFixture struct {
	repo       string
	index      domain.StagingIndex
	position   domain.WorkingPosition
	pending    []domain.Pending
	commits    []domain.StagingCommit
	history    domain.HistoryQueryResult
	snapshots  map[domain.ContentHash]domain.Snapshot
	docs       map[domain.ContentHash]domain.SessionDoc
	onDoc      func()
	onSnapshot func()
}

func (f *workingReadFixture) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return domain.Repo{ID: f.repo}, nil
}
func (f *workingReadFixture) CurrentBranch(context.Context, string) (string, error) {
	return "main", nil
}
func (f *workingReadFixture) CurrentCommit(context.Context, string) (string, error) {
	return strings.Repeat("a", 40), nil
}
func (f *workingReadFixture) ReadStaging(context.Context, string) (domain.StagingIndex, domain.WorkingPosition, error) {
	return f.index, f.position, nil
}
func (f *workingReadFixture) ListStagingCommits(context.Context, string) ([]domain.StagingCommit, error) {
	return f.commits, nil
}
func (f *workingReadFixture) ListPendings(context.Context, string) ([]domain.Pending, error) {
	return f.pending, nil
}
func (f *workingReadFixture) GetSnapshot(_ context.Context, h domain.ContentHash) (domain.Snapshot, error) {
	if f.onSnapshot != nil {
		f.onSnapshot()
	}
	v, ok := f.snapshots[h]
	if !ok {
		return v, domain.ErrNotFound
	}
	return v, nil
}
func (f *workingReadFixture) GetDoc(_ context.Context, h domain.ContentHash) (domain.SessionDoc, error) {
	if f.onDoc != nil {
		f.onDoc()
	}
	v, ok := f.docs[h]
	if !ok {
		return v, domain.ErrNotFound
	}
	return v, nil
}
func (f *workingReadFixture) QueryHistory(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return f.history, nil
}
func workingDoc(t *testing.T, session string, n int) domain.SessionDoc {
	t.Helper()
	d := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderCodex, SessionOriginID: session, CIRVersion: domain.CIRVersionV1}}}
	for i := 0; i < n; i++ {
		d.CIR.Events = append(d.CIR.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "PRIVATE_DO_NOT_PRINT"}}})
	}
	raw, err := domain.CanonicalBytes(d.CIR)
	if err != nil {
		t.Fatal(err)
	}
	d.Hash = domain.HashContent(raw)
	return d
}
func newWorkingReadFixture(t *testing.T) (*WorkingStateService, *workingReadFixture) {
	t.Helper()
	repo := string(domain.HashContent([]byte("repo")))
	f := &workingReadFixture{repo: repo, snapshots: map[domain.ContentHash]domain.Snapshot{}, docs: map[domain.ContentHash]domain.SessionDoc{}}
	for n := 1; n <= 4; n++ {
		d := workingDoc(t, "session", n)
		f.docs[d.Hash] = d
	}
	base := workingDoc(t, "session", 1)
	f.snapshots[base.Hash] = domain.Snapshot{ID: base.Hash, DocHash: base.Hash, RepoID: repo, Provider: domain.ProviderCodex, SessionID: "session", MemoryHash: domain.HashContent([]byte("new-memory"))}
	f.position = domain.WorkingPosition{RepoID: repo, WorktreeID: strings.Repeat("a", 32), GitCommit: strings.Repeat("a", 40), Branch: "main", BranchID: "main-id", Snapshot: base.Hash, MemorySource: base.Hash, MemoryHash: domain.HashContent([]byte("old-memory")), MemoryPinned: true}
	f.index = (domain.StagingIndex{Version: domain.StagingVersion, RepoID: repo, WorktreeID: f.position.WorktreeID, Entries: []domain.StagedSession{}}).WithRevision()
	f.history = domain.HistoryQueryResult{Version: domain.QueryContractVersion, Position: base.Hash, Complete: true, StateHash: domain.HashContent([]byte("history")), Snapshots: []domain.Snapshot{f.snapshots[base.Hash]}}
	return NewWorkingStateService(f, f, f, f, f), f
}
func (f *workingReadFixture) entry(t *testing.T, n int) domain.StagedSession {
	t.Helper()
	doc := workingDoc(t, "session", n)
	e := domain.StagedSession{Provider: domain.ProviderCodex, SessionID: "session", SourceID: domain.HashContent([]byte("source")), Generation: workingDoc(t, "session", 1).Hash, DocHash: doc.Hash, Events: n, CodeCommit: f.position.GitCommit, Branch: "main", BranchID: "main-id", CapturedAt: time.Unix(100, 0).UTC()}
	e.Key = domain.StagedSessionKey(e.Provider, e.SessionID, e.SourceID, e.Generation)
	return e
}
func TestWorkingStatePinsMemoryAndDoesNotClaimIdleOrRemoteSync(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	out, err := s.Status(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Memory.AttachmentChanged || out.Memory.AppliedHash != f.position.MemoryHash || out.Memory.ObservedAttachment != f.snapshots[f.position.Snapshot].MemoryHash {
		t.Fatal(out.Memory)
	}
	if out.Freshness.ServerChecked || out.Freshness.WatcherState != "unknown" || out.Freshness.LastCaptureAt != nil || len(out.Pending) != 0 {
		t.Fatal(out)
	}
	previous := out.Revision
	snap := f.snapshots[f.position.Snapshot]
	snap.MemoryHash = domain.HashContent([]byte("next-memory"))
	f.snapshots[snap.ID] = snap
	out, err = s.Status(context.Background(), "")
	if err != nil || out.Revision == previous || out.Memory.AppliedHash != f.position.MemoryHash {
		t.Fatal(out, err)
	}
	f.position.GitCommit = strings.Repeat("b", 40)
	out, err = s.Status(context.Background(), "")
	if err != nil || out.Selection.CodeMatchesSelection {
		t.Fatal(out, err)
	}
}
func TestWorkingStateRejectsContinuouslyChangingObservation(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	n := 0
	f.onSnapshot = func() {
		n++
		snap := f.snapshots[f.position.Snapshot]
		snap.MemoryHash = domain.HashContent([]byte(strings.Repeat("x", n)))
		f.snapshots[snap.ID] = snap
	}
	if _, err := s.Status(context.Background(), ""); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal(err)
	}
}
func TestWorkingStateReadsAbsentStorageWithoutCreatingOrRepairing(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	root := t.TempDir()
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", f.position.GitCommit)
	f.history.Position = ""
	f.history.Snapshots = nil
	s.store = st
	s.docs = st
	s.WithAppliedPullReader(st, "https://selected.test/api/v1")
	if _, err := s.Status(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Diff(context.Background(), inbound.ContextDiffInput{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("read created state", entries, err)
	}
}
func TestWorkingStateDiagnosticsOmitRawHistoryAndCreationCommands(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	snapshot := f.history.Snapshots[0]
	snapshot.Message = "PRIVATE_DO_NOT_PRINT"
	f.history.Snapshots = []domain.Snapshot{snapshot}
	f.position.Selection = &domain.HistoryEvent{ID: "PRIVATE_DO_NOT_PRINT"}
	out, err := s.Status(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "PRIVATE_DO_NOT_PRINT") {
		t.Fatal("raw diagnostics leaked")
	}
}

func TestWorkingStateLeavesInterruptedJournalUntouched(t *testing.T) {
	s, f := newWorkingReadFixture(t)
	root := t.TempDir()
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", f.position.GitCommit)
	f.history.Position = ""
	f.history.Snapshots = nil
	s.store = st
	s.docs = st
	if err := os.Mkdir(filepath.Join(root, ".cxt"), 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, ".cxt", "working-commit.json")
	original := []byte("interrupted journal requires explicit repair")
	if err := os.WriteFile(journal, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Diff(context.Background(), inbound.ContextDiffInput{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(after) != string(original) {
		t.Fatal("inspection repaired journal", string(after), err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".cxt"))
	if err != nil || len(entries) != 1 {
		t.Fatal("inspection created index/lock", entries, err)
	}
}

type workingAppliedPullReader func(context.Context, string, string) (outbound.SelectedPullReceipt, error)

func (read workingAppliedPullReader) ReadAppliedPull(ctx context.Context, repo, remote string) (outbound.SelectedPullReceipt, error) {
	return read(ctx, repo, remote)
}

func workingPullReceipt(f *workingReadFixture) outbound.SelectedPullReceipt {
	position := f.position
	plan := outbound.SelectedPullPlan{
		Version: 1, RepoID: f.repo, Remote: "https://selected.test/api/v1",
		Expected:  outbound.CheckoutState{Position: &position, IndexRevision: f.index.Revision},
		Selection: domain.ContextSelection{Branch: position.Branch, Position: string(position.Snapshot), CodeCommit: position.GitCommit, Scope: "current"},
		Context:   domain.ContextQueryView{StateHash: domain.HashContent([]byte("applied context"))},
		Memory:    []domain.EffectiveMemoryPage{{StateHash: domain.HashContent([]byte("applied memory"))}},
	}
	plan.ID = outbound.SelectedPullPlanID(plan)
	return outbound.SelectedPullReceipt{Plan: plan, AppliedAt: time.Unix(200, 0).UTC()}
}

func TestWorkingStateAppliedPullSummaryAndStaleSelection(t *testing.T) {
	for _, kind := range []string{"current", "snapshot", "context-branch", "branch-identity", "selected-code", "receipt-code", "git-branch", "receipt-git-branch", "no-memory"} {
		t.Run(kind, func(t *testing.T) {
			s, f := newWorkingReadFixture(t)
			receipt := workingPullReceipt(f)
			switch kind {
			case "snapshot":
				f.position.Snapshot = domain.HashContent([]byte("next selection"))
				f.history.Position = f.position.Snapshot
			case "context-branch":
				f.position.Branch, f.position.LocalBranch = "feature", "main"
			case "branch-identity":
				f.position.BranchID = "recreated-main"
			case "selected-code":
				f.position.GitCommit = strings.Repeat("b", 40)
			case "receipt-code":
				receipt.Plan.Selection.CodeCommit = strings.Repeat("b", 40)
				receipt.Plan.Expected.Position.GitCommit = receipt.Plan.Selection.CodeCommit
			case "git-branch":
				f.position.LocalBranch = "feature"
			case "receipt-git-branch":
				receipt.Plan.Expected.Position.LocalBranch = "previous-git-branch"
			case "no-memory":
				receipt.Plan.Memory = nil
			}
			receipt.Plan.ID = outbound.SelectedPullPlanID(receipt.Plan)
			before, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.WithAppliedPullReader(workingAppliedPullReader(func(_ context.Context, repo, remote string) (outbound.SelectedPullReceipt, error) {
				calls++
				if repo != f.repo || remote != receipt.Plan.Remote {
					t.Fatalf("receipt read for wrong scope: repo=%q remote=%q", repo, remote)
				}
				return receipt, nil
			}), receipt.Plan.Remote)
			out, err := s.Status(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			current := kind == "current" || kind == "no-memory"
			want := domain.AppliedProjectionSummary{ReceiptID: receipt.Plan.ID, ContextStateHash: receipt.Plan.Context.StateHash, GitCommit: receipt.Plan.Selection.CodeCommit, AppliedAt: receipt.AppliedAt, MatchesSelection: current}
			if len(receipt.Plan.Memory) > 0 {
				want.MemoryStateHash = receipt.Plan.Memory[0].StateHash
			}
			if out.AppliedProjection == nil || *out.AppliedProjection != want {
				t.Fatalf("applied projection = %+v, want %+v", out.AppliedProjection, want)
			}
			if slices.Contains(out.Gaps, "applied_projection_for_previous_selection") == current {
				t.Fatalf("stale receipt gap incorrect: %v", out.Gaps)
			}
			if out.Selection.RepoID != f.repo || out.Selection.WorktreeID != f.position.WorktreeID || out.Freshness.ServerChecked || calls < 2 {
				t.Fatalf("receipt bypassed scoped local observation: %+v; reads=%d", out, calls)
			}
			after, err := json.Marshal(receipt)
			if err != nil || string(after) != string(before) {
				t.Fatalf("status mutated stored receipt: %s; %v", after, err)
			}
		})
	}
}

func TestWorkingStateAppliedPullRejectsInvalidScopeAndIdentity(t *testing.T) {
	for _, kind := range []string{"repo", "remote", "worktree", "missing-position", "plan-id"} {
		t.Run(kind, func(t *testing.T) {
			s, f := newWorkingReadFixture(t)
			receipt := workingPullReceipt(f)
			remote := receipt.Plan.Remote
			switch kind {
			case "repo":
				receipt.Plan.RepoID = string(domain.HashContent([]byte("other repository")))
			case "remote":
				receipt.Plan.Remote = "https://other.test/api/v1"
			case "worktree":
				receipt.Plan.Expected.Position.WorktreeID = strings.Repeat("b", 32)
			case "missing-position":
				receipt.Plan.Expected.Position = nil
			}
			receipt.Plan.ID = outbound.SelectedPullPlanID(receipt.Plan)
			if kind == "plan-id" {
				receipt.Plan.ID = domain.HashContent([]byte("tampered receipt"))
			}
			s.WithAppliedPullReader(workingAppliedPullReader(func(context.Context, string, string) (outbound.SelectedPullReceipt, error) {
				return receipt, nil
			}), remote)
			out, err := s.Status(context.Background(), "")
			if !errors.Is(err, domain.ErrHashMismatch) || out.AppliedProjection != nil {
				t.Fatalf("invalid receipt accepted: %+v; %v", out.AppliedProjection, err)
			}
		})
	}
}

func TestWorkingStateAppliedPullReaderErrors(t *testing.T) {
	for _, readErr := range []error{fmt.Errorf("missing receipt: %w", domain.ErrNotFound), os.ErrPermission, domain.ErrHashMismatch, context.Canceled} {
		t.Run(readErr.Error(), func(t *testing.T) {
			s, f := newWorkingReadFixture(t)
			receipt := workingPullReceipt(f)
			s.WithAppliedPullReader(workingAppliedPullReader(func(context.Context, string, string) (outbound.SelectedPullReceipt, error) {
				return receipt, readErr // Partial data must never be reported on error.
			}), receipt.Plan.Remote)
			out, err := s.Status(context.Background(), "")
			if errors.Is(readErr, domain.ErrNotFound) {
				if err != nil || slices.Contains(out.Gaps, "applied_projection_for_previous_selection") {
					t.Fatalf("absent receipt treated as stale or failed: %+v; %v", out, err)
				}
			} else if !errors.Is(err, readErr) {
				t.Fatalf("reader error = %v, want %v", err, readErr)
			}
			if out.AppliedProjection != nil {
				t.Fatalf("reported receipt despite read error: %+v", out.AppliedProjection)
			}
		})
	}
	for _, kind := range []string{"no-reader", "no-remote", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newWorkingReadFixture(t)
			reader := workingAppliedPullReader(func(context.Context, string, string) (outbound.SelectedPullReceipt, error) {
				t.Fatal("unexpected receipt read")
				return outbound.SelectedPullReceipt{}, nil
			})
			ctx := context.Background()
			switch kind {
			case "no-reader":
				s.WithAppliedPullReader(nil, "https://selected.test/api/v1")
			case "no-remote":
				s.WithAppliedPullReader(reader, "")
			case "canceled":
				s.WithAppliedPullReader(reader, "https://selected.test/api/v1")
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			out, err := s.Status(ctx, "")
			if (kind == "canceled" && !errors.Is(err, context.Canceled)) || (kind != "canceled" && err != nil) || out.AppliedProjection != nil {
				t.Fatalf("unexpected status: %+v; %v", out, err)
			}
		})
	}
}

func TestWorkingStateAppliedPullParticipatesInReadFence(t *testing.T) {
	for _, continuous := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuous=%t", continuous), func(t *testing.T) {
			s, f := newWorkingReadFixture(t)
			receipt := workingPullReceipt(f)
			calls := 0
			s.WithAppliedPullReader(workingAppliedPullReader(func(context.Context, string, string) (outbound.SelectedPullReceipt, error) {
				calls++
				if continuous || calls == 2 {
					receipt.AppliedAt = receipt.AppliedAt.Add(time.Second)
				}
				return receipt, nil
			}), receipt.Plan.Remote)
			out, err := s.Status(context.Background(), "")
			if continuous {
				if !errors.Is(err, domain.ErrSelectionChanged) || out.AppliedProjection != nil {
					t.Fatalf("unstable receipt reported: %+v; %v", out, err)
				}
			} else if err != nil || calls < 3 || out.AppliedProjection == nil || out.AppliedProjection.AppliedAt != receipt.AppliedAt {
				t.Fatalf("did not retry changed receipt: reads=%d, %+v; %v", calls, out, err)
			}
		})
	}
}

func TestWorkingStateAppliedPullStorageScopeAndNoMutation(t *testing.T) {
	ctx := context.Background()
	pull, st, _, _, git, plan := selectedPullFixture(t)
	receipt, err := pull.Apply(ctx, git.repo.LocalPath, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, history := newWorkingReadFixture(t)
	history.history.Position = plan.Expected.Position.Snapshot
	history.history.Snapshots = plan.Context.Snapshots
	// A read must leave even an interrupted write journal for explicit repair.
	if err := os.WriteFile(filepath.Join(git.repo.LocalPath, ".cxt", "working-commit.json"), []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshotTree := func() map[string]string {
		t.Helper()
		files := map[string]string{}
		err := filepath.WalkDir(git.repo.LocalPath, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files[path] = fmt.Sprintf("%s %s", info.Mode(), info.ModTime())
			if !entry.IsDir() {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] += string(body)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshotTree()
	for _, kind := range []string{"selected", "other-remote", "other-worktree", "other-repo"} {
		t.Run(kind, func(t *testing.T) {
			reader, remote := st, plan.Remote
			switch kind {
			case "other-remote":
				remote = "https://other.test/api/v1"
			case "other-worktree":
				reader = storage.NewWorktreeFileStore(git.repo.LocalPath, filepath.Join(git.repo.LocalPath, ".git", "worktrees", "other"), git.branch, git.sha)
			case "other-repo":
				if _, err := st.ReadAppliedPull(ctx, string(domain.HashContent([]byte("other repo"))), remote); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("receipt leaked across repositories: %v", err)
				}
				return
			}
			s := NewWorkingStateService(git, git, st, st, history).WithAppliedPullReader(reader, remote)
			out, err := s.Status(ctx, git.repo.LocalPath)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "selected" {
				if out.AppliedProjection == nil || out.AppliedProjection.ReceiptID != receipt.Plan.ID || !out.AppliedProjection.MatchesSelection {
					t.Fatalf("selected receipt missing: %+v", out.AppliedProjection)
				}
			} else if out.AppliedProjection != nil {
				t.Fatalf("receipt leaked outside its scope: %+v", out.AppliedProjection)
			}
		})
	}
	if after := snapshotTree(); !reflect.DeepEqual(after, before) {
		t.Fatal("status changed stored files, timestamps, or created repair/lock state")
	}
}

func TestWorkingStateStagingCommitVersions(t *testing.T) {
	for _, version := range []int{1, domain.StagingCommitVersion, domain.StagingCommitVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s, f := newWorkingReadFixture(t)
			f.commits = []domain.StagingCommit{{Version: version, ID: "versioned", Index: f.index, Position: f.position, LocalFinalized: true}}
			_, err := s.Status(context.Background(), "")
			if version > domain.StagingCommitVersion {
				if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("unknown receipt accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
