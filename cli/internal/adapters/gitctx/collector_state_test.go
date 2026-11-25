package gitctx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestCollectorStateIsAMemoryThatMayBeLost(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	files := NewCollectorFiles()
	if state, err := files.LoadCollectorState(ctx, repo); err != nil || !reflect.DeepEqual(state, domain.CollectorState{}) {
		t.Fatalf("no state yet: %+v %v", state, err)
	}
	state := domain.CollectorState{Version: domain.CollectorStateVersion, EnabledAt: "2031-01-02T03:04:05Z", Target: "repository",
		Sessions: map[string]domain.CollectorSessionState{"claude/s1": {Sent: "fingerprint", ContentKey: "key",
			Content: &domain.AnchoredContent{Events: 3, Parts: []domain.AnchoredContentPart{{Doc: domain.HashContent([]byte("doc")), Events: 3}}}, FirstEvent: []int{0, 2}, Events: []int{2, 1}}},
		Branches: "branches", LastRun: &domain.CollectorRun{At: "2031-01-02T03:05:00Z", Stored: 1}}
	if err := files.SaveCollectorState(ctx, repo, state); err != nil {
		t.Fatal(err)
	}
	if got, err := files.LoadCollectorState(ctx, repo); err != nil || !reflect.DeepEqual(got, state) {
		t.Fatalf("saved state:\n got %+v\nwant %+v\n%v", got, state, err)
	}
	// It lives beside Git's files: every worktree shares it, none shows it.
	linked := filepath.Join(filepath.Dir(repo), "linked")
	branchIDTestGit(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	if got, err := files.LoadCollectorState(ctx, linked); err != nil || got.Target != "repository" {
		t.Fatalf("from a linked worktree: %+v %v", got, err)
	}
	if status := branchIDTestGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("the state shows in the working tree: %s", status)
	}
	path := filepath.Join(repo, ".git", "cxt", collectorStateFile)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", info, err)
	}
	// A state that cannot be read, or of another layout, is an empty one.
	for _, content := range []string{"{not json", `{"version":99,"enabled_at":"2031-01-02T03:04:05Z"}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := files.LoadCollectorState(ctx, repo); err != nil || !reflect.DeepEqual(got, domain.CollectorState{}) {
			t.Fatalf("after %q: %+v %v", content, got, err)
		}
	}
	if _, err := files.LoadCollectorState(ctx, t.TempDir()); err == nil {
		t.Fatal("a folder that is not a repository must fail")
	}
}

// A push that ended is noted once and taken once, however often it is noted.
func TestCollectorPushIsNotedAndTakenOnce(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	files := NewCollectorFiles()
	if files.TakePush(ctx, repo) {
		t.Fatal("a push was taken before any was noted")
	}
	files.MarkPush(ctx, repo)
	files.MarkPush(ctx, repo)
	if !files.TakePush(ctx, repo) {
		t.Fatal("a noted push was not taken")
	}
	if files.TakePush(ctx, repo) {
		t.Fatal("one push was taken twice")
	}
	// The note is kept beside Git's own files, where the collector keeps its
	// state, and nowhere in the working tree.
	files.MarkPush(ctx, repo)
	dir, err := collectorDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, collectorPushedFile)); err != nil {
		t.Fatalf("the note is not where the collector keeps its files: %v", err)
	}
	// The note of a push is not a request for one more run: a collector that
	// starts takes the requests left for it and leaves the note where it is.
	requested := true
	if ran, err := files.WithCollectorLock(ctx, repo, func(another func() bool) error { requested = another(); return nil }); err != nil || !ran || requested {
		t.Fatalf("a collector beside the note of a push: ran=%v asked again=%v err=%v", ran, requested, err)
	}
	if !files.TakePush(ctx, repo) {
		t.Fatal("the collector's lock took the note of a push")
	}
	// Outside a repository there is no collector: nothing is noted or taken.
	outside := t.TempDir()
	files.MarkPush(ctx, outside)
	if files.TakePush(ctx, outside) {
		t.Fatal("a push was taken outside a repository")
	}
}

func TestCollectorLockLetsOneRunAndKeepsOneRequest(t *testing.T) {
	repo := branchIDTestRepo(t)
	ctx := context.Background()
	files := NewCollectorFiles()
	entered, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		ran      bool
		requests []bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		var requests []bool
		ran, err := files.WithCollectorLock(ctx, repo, func(another func() bool) error {
			requests = append(requests, another()) // nothing asked yet
			close(entered)
			<-release
			requests = append(requests, another(), another()) // asked twice meanwhile: one request
			return nil
		})
		done <- outcome{ran, requests, err}
	}()
	<-entered
	for attempt := 0; attempt < 2; attempt++ {
		ran, err := files.WithCollectorLock(ctx, repo, func(func() bool) error {
			t.Error("a second collector ran")
			return nil
		})
		if ran || err != nil {
			t.Fatalf("while one is running: ran=%v err=%v", ran, err)
		}
	}
	// A hook that finds a collector running leaves its request without
	// starting one.
	if !files.RequestRunIfBusy(ctx, repo) {
		t.Fatal("a running collector was not seen")
	}
	close(release)
	got := <-done
	if !got.ran || got.err != nil || !reflect.DeepEqual(got.requests, []bool{false, true, false}) {
		t.Fatalf("the running collector: %+v", got)
	}
	// With none running there is nothing to leave a request for.
	if files.RequestRunIfBusy(ctx, repo) {
		t.Fatal("a collector was seen where none runs")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "cxt", collectorRequestFile)); !os.IsNotExist(err) {
		t.Fatalf("a request was left for nobody: %v", err)
	}
	if files.RequestRunIfBusy(ctx, t.TempDir()) {
		t.Fatal("a folder that is not a repository has no collector")
	}
	// A request left behind is answered by the next collector's own first run.
	if err := os.WriteFile(filepath.Join(repo, ".git", "cxt", collectorRequestFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic failure")
	ran, err := files.WithCollectorLock(ctx, repo, func(another func() bool) error {
		if another() {
			t.Error("a request from before the run was kept for a second run")
		}
		return failure
	})
	if !ran || !errors.Is(err, failure) {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if status := branchIDTestGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("collector files show in the working tree: %s", status)
	}
}
