package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func historyFixture(t *testing.T) (string, *Container, *storage.FileStore, string, domain.ContentHash) {
	t.Helper()
	cwd := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	runLifecycleGit(t, cwd, "init", "-q", "-b", "main")
	runLifecycleGit(t, cwd, "config", "core.hooksPath", "/dev/null")
	runLifecycleGit(t, cwd, "config", "commit.gpgsign", "false")
	runLifecycleGit(t, cwd, "config", "user.name", "test")
	runLifecycleGit(t, cwd, "config", "user.email", "test@example.test")
	runLifecycleGit(t, cwd, "commit", "--allow-empty", "-qm", "base")
	ctx := context.Background()
	repo, err := gitctx.NewGitContextAdapter().CurrentRepo(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewFileStore(cwd)
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	id, err := store.PutDoc(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo.ID, Branch: "main", Message: "baseline"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: id}); err != nil {
		t.Fatal(err)
	}
	if err = store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: "main"}); err != nil {
		t.Fatal(err)
	}
	c := &Container{List: app.NewListSessionsService(store), Fork: app.NewForkSessionService(store), History: app.NewContextHistoryService(store, store)}
	return cwd, c, store, repo.ID, id
}

func runBirthVote(t *testing.T, cwd string, c *Container, phase, line string) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
	_, _ = f.Seek(0, 0)
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	return runBranchTransaction(context.Background(), c, cwd, []string{phase})
}

func TestBranchBirthReplaysAfterPreparedOnlyInterruption(t *testing.T) {
	cwd, c, store, repoID, source := historyFixture(t)
	t.Setenv("CXT_KEEP_SESSION", "1")
	oid := gitOut(cwd, "rev-parse", "HEAD")
	line := strings.Repeat("0", 40) + " " + oid + " refs/heads/feature/durable"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	before, err := j.List()
	if err != nil || len(before) != 1 {
		t.Fatalf("journal: %v %v", before, err)
	}
	// Git commits, but the process crashes before delivering committed.
	runLifecycleGit(t, cwd, "branch", "feature/durable")
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListHistoryEvents(context.Background(), repoID)
	if err != nil || len(events) != 1 || events[0].ID != before[0].Event.ID || events[0].Source != source {
		t.Fatalf("replay: %+v %v", events, err)
	}
	ref, err := store.GetRef(context.Background(), repoID, domain.RefBranch, "feature/durable")
	if err != nil || ref.Target != source {
		t.Fatalf("branch: %+v %v", ref, err)
	}
	if gitOut(cwd, "symbolic-ref", "--short", "HEAD") != "main" {
		t.Fatal("background creation switched Git")
	}
	head, _ := store.GetRef(context.Background(), repoID, domain.RefHEAD, "HEAD")
	if head.Symbolic != "main" {
		t.Fatal("background creation switched context")
	}
	after, _ := j.List()
	if after[0].Phase != "applied" {
		t.Fatalf("phase=%s", after[0].Phase)
	}
}

func TestBranchBirthAbortedTransactionDoesNotCreateContext(t *testing.T) {
	cwd, c, store, repoID, _ := historyFixture(t)
	line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/rejected"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	if err := runBirthVote(t, cwd, c, "aborted", line); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ListHistoryEvents(context.Background(), repoID)
	if len(events) != 0 {
		t.Fatal("aborted birth published")
	}
}

func TestBranchBirthRejectsDamagedReplicaAndJournal(t *testing.T) {
	for _, damage := range []string{"replica", "journal", "doc"} {
		t.Run(damage, func(t *testing.T) {
			cwd, c, _, _, source := historyFixture(t)
			j, _ := branchjournal.Open(context.Background(), cwd)
			if err := j.Enable(); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "replica":
				if err := os.Rename(filepath.Join(cwd, ".cxt"), filepath.Join(cwd, ".cxt-backup")); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err := os.WriteFile(filepath.Join(cwd, ".git", "cxt", "operations"), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "doc":
				if err := os.WriteFile(filepath.Join(cwd, ".cxt", "objects", "docs", strings.TrimPrefix(string(source), "sha256:")), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			line := strings.Repeat("0", 40) + " " + gitOut(cwd, "rev-parse", "HEAD") + " refs/heads/rejected"
			if err := runBirthVote(t, cwd, c, "prepared", line); err == nil {
				t.Fatal("damaged state accepted")
			}
		})
	}
}

func TestOrphanBirthRecordsMemoryProvenanceWithoutConversationParent(t *testing.T) {
	cwd, c, _, _, source := historyFixture(t)
	line := strings.Repeat("0", 40) + " ref:refs/heads/orphan-x HEAD"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	ops, err := j.List()
	if err != nil || len(ops) != 1 {
		t.Fatalf("journal: %+v %v", ops, err)
	}
	if ops[0].Event.Source != "" || ops[0].Event.MemorySource != source || ops[0].Event.Kind != "orphan" {
		t.Fatalf("orphan=%+v", ops[0].Event)
	}
}

func TestExistingSymbolicHeadChangeIsNotAnOrphanBirth(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	runLifecycleGit(t, cwd, "branch", "existing")
	line := strings.Repeat("0", 40) + " ref:refs/heads/existing HEAD"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(context.Background(), cwd)
	ops, err := j.List()
	if err != nil || len(ops) != 0 {
		t.Fatalf("ordinary checkout recorded an orphan: %+v %v", ops, err)
	}
}

func TestEmptyBranchReplayDoesNotBindPreRemoteRepositoryIdentity(t *testing.T) {
	cwd, c, _, _, _ := historyFixture(t)
	j, err := branchjournal.Open(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), c, cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git", "cxt", "repository")); !os.IsNotExist(err) {
		t.Fatalf("empty replay bound a repository before remote setup: %v", err)
	}
}

type waitingTrackingSync struct {
	inbound.SyncRepo
	entered chan struct{}
	release chan struct{}
	target  domain.Ref
}

func (s waitingTrackingSync) ResolveRemoteBranch(ctx context.Context, _ inbound.SyncInput, _ string) (domain.Ref, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.target, nil
	case <-ctx.Done():
		return domain.Ref{}, ctx.Err()
	}
}

func TestTrackingReplayDoesNotHoldTheGitCreationVoteDuringNetwork(t *testing.T) {
	cwd, c, store, repoID, source := historyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A remote-tracking ref and mapping suffice; no network or ambient auth is used.
	oid := gitOut(cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/team-task", oid)
	known := domain.HistoryEvent{ID: strings.Repeat("a", 32), BranchID: "teammate-logical-branch", RepoID: repoID, Branch: "team-task", Kind: "birth", GitAfter: oid, Source: source, Target: source, CreatedAt: time.Now().UTC()}
	if err := store.PutHistoryEvent(ctx, known); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("0", 40) + " " + oid + " refs/heads/team-task"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "branch", "--track", "team-task", "origin/team-task")
	entered, release := make(chan struct{}), make(chan struct{})
	c.Sync = waitingTrackingSync{entered: entered, release: release, target: domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: "team-task", Target: source}}
	result := make(chan error, 1)
	go func() { result <- replayBranchOperations(ctx, c, cwd) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("tracking resolution never started")
	}
	j, _ := branchjournal.Open(ctx, cwd)
	voteCtx, voteCancel := context.WithTimeout(ctx, time.Second)
	err := j.Transaction(voteCtx, func() error { return nil })
	voteCancel()
	if err != nil {
		close(release)
		t.Fatalf("remote lookup blocked the prepared vote: %v", err)
	}
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/independent"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	events, err := store.ListHistoryEvents(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	attached := false
	for _, e := range events {
		if e.Kind == "attach" {
			attached = true
			if e.BranchID != known.BranchID || e.Target != source {
				t.Fatalf("tracking created another logical branch: %+v", e)
			}
		}
	}
	if !attached {
		t.Fatal("missing tracking attachment")
	}
}

func TestCodeSelectionUsesRecordedMemoryIncludingExplicitlyEmpty(t *testing.T) {
	cwd, _, store, repoID, id := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	snapshots, err := store.ListSnapshots(context.Background(), repoID, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshots[0].Message = "captured [git " + code + "]"
	snapshots[0].MemoryHash = domain.HashContent([]byte("new memory"))
	snapshots[0].CreatedAt = time.Unix(3, 0)
	for _, memory := range []domain.ContentHash{"", domain.HashContent([]byte("old memory"))} {
		event := domain.HistoryEvent{Kind: "position", Branch: "main", Target: id, MemoryHash: memory, MemoryPinned: true, GitAfter: code, CreatedAt: time.Unix(2, 0)}
		selected := contextSelectionAtCode(cwd, code, "main", snapshots, []domain.HistoryEvent{event})
		if selected.Snapshot != id || selected.MemoryHash != memory || !selected.MemoryPinned {
			t.Fatalf("selected current attachment instead of recorded code version: %+v", selected)
		}
	}
}

func TestCodeSelectionFollowsIdentityThroughRenameAndReusedName(t *testing.T) {
	cwd, _, _, repo, id := historyFixture(t)
	code := gitOut(cwd, "rev-parse", "HEAD")
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "original", Branch: "old", Kind: "birth", Target: id, GitAfter: code, MemoryPinned: true}
	rename := birth
	rename.ID, rename.Kind, rename.Branch, rename.PreviousBranch, rename.BindingParent = strings.Repeat("b", 32), "rename", "new", "old", birth.ID
	rename.GitAfter = ""
	reuse := birth
	reuse.ID, reuse.BranchID, reuse.BindingParent, reuse.Target = strings.Repeat("c", 32), "reused", rename.ID, domain.HashContent([]byte("different task"))
	reuse.CreatedAt = time.Now().UTC()
	history := []domain.HistoryEvent{reuse, rename, birth}
	for _, name := range []string{"new", "old"} {
		got := contextSelectionAtCode(cwd, code, name, nil, history)
		want := id
		if name == "old" {
			want = reuse.Target
		}
		if got.Snapshot != want || !got.MemoryPinned {
			t.Fatalf("%s selection mixed identities: %+v", name, got)
		}
	}
}
