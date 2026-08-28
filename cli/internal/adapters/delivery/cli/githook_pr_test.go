package cli

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type fakePRMergeResolver struct {
	pulls  []outbound.MergedPullRequest
	err    error
	remote string
	base   string
	shas   []string
}

func (f *fakePRMergeResolver) ResolveMergedPullRequests(
	_ context.Context,
	remote, base string,
	shas []string,
) ([]outbound.MergedPullRequest, error) {
	f.remote = remote
	f.base = base
	f.shas = append([]string(nil), shas...)
	return append([]outbound.MergedPullRequest(nil), f.pulls...), f.err
}

type appendCall struct {
	branch string
	target domain.ContentHash
}

type fakeMergedPRSync struct {
	refs        map[string]domain.Ref
	resolveErrs map[string]error
	appendErrs  map[domain.ContentHash]error
	appends     []appendCall
}

func (f *fakeMergedPRSync) ResolveRemoteBranch(
	_ context.Context,
	_ inbound.SyncInput,
	branch string,
) (domain.Ref, error) {
	if err := f.resolveErrs[branch]; err != nil {
		return domain.Ref{}, err
	}
	if ref, ok := f.refs[branch]; ok {
		return ref, nil
	}
	return domain.Ref{}, domain.ErrNotFound
}

func (f *fakeMergedPRSync) AppendBranch(
	_ context.Context,
	_ inbound.SyncInput,
	branch string,
	target domain.ContentHash,
) error {
	f.appends = append(f.appends, appendCall{branch: branch, target: target})
	return f.appendErrs[target]
}

func TestAppendMergedPRContextsPromotesInResolverOrder(t *testing.T) {
	t.Parallel()

	resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{
		{Number: 11, BaseBranch: "main", HeadBranch: "feature/old", MergeCommitSHA: "aaa"},
		{Number: 12, BaseBranch: "release", HeadBranch: "wrong-base", MergeCommitSHA: "bbb"},
		{Number: 13, BaseBranch: "main", HeadBranch: "missing", MergeCommitSHA: "ccc"},
		{Number: 14, BaseBranch: "main", HeadBranch: "feature/already", MergeCommitSHA: "ddd"},
		{Number: 15, BaseBranch: "main", HeadBranch: "feature/new", MergeCommitSHA: "eee"},
	}}
	oldTarget := domain.ContentHash("sha256:old")
	alreadyTarget := domain.ContentHash("sha256:already")
	newTarget := domain.ContentHash("sha256:new")
	syncer := &fakeMergedPRSync{
		refs: map[string]domain.Ref{
			"feature/old":     {Target: oldTarget},
			"feature/already": {Target: alreadyTarget},
			"feature/new":     {Target: newTarget},
		},
		appendErrs: map[domain.ContentHash]error{
			alreadyTarget: errors.New("non_fast_forward: already reachable"),
		},
	}

	shas := []string{"aaa", "bbb", "ccc", "ddd", "eee"}
	reflected := appendMergedPRContexts(
		context.Background(),
		resolver,
		syncer,
		"/repo",
		"main",
		"git@github.com:acme/project.git",
		shas,
	)
	if !reflected {
		t.Fatal("successful/already-promoted PR contexts were not reported as reflected")
	}

	if resolver.remote != "git@github.com:acme/project.git" || resolver.base != "main" {
		t.Fatalf("resolver inputs = (%q, %q), want Git remote and main", resolver.remote, resolver.base)
	}
	if strings.Join(resolver.shas, ",") != strings.Join(shas, ",") {
		t.Fatalf("resolver SHAs = %v, want %v", resolver.shas, shas)
	}
	want := []appendCall{
		{branch: "main", target: oldTarget},
		{branch: "main", target: alreadyTarget},
		{branch: "main", target: newTarget},
	}
	if len(syncer.appends) != len(want) {
		t.Fatalf("append calls = %#v, want %#v", syncer.appends, want)
	}
	for i := range want {
		if syncer.appends[i] != want[i] {
			t.Fatalf("append call %d = %#v, want %#v", i, syncer.appends[i], want[i])
		}
	}
}

func TestAppendMergedPRContextsResolverFailureIsFailOpen(t *testing.T) {
	t.Parallel()

	resolver := &fakePRMergeResolver{err: errors.New("rate limited")}
	syncer := &fakeMergedPRSync{}
	reflected := appendMergedPRContexts(
		context.Background(),
		resolver,
		syncer,
		"/repo",
		"main",
		"https://github.com/acme/project",
		[]string{"aaa"},
	)
	if reflected {
		t.Fatal("resolver failure was reported as reflected")
	}
	if len(syncer.appends) != 0 {
		t.Fatalf("append calls = %#v, want none after resolver failure", syncer.appends)
	}
}

func TestAppendMergedPRContextsPromotionFailureIsNotReflected(t *testing.T) {
	t.Parallel()

	target := domain.ContentHash("sha256:failed")
	resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{
		Number: 21, BaseBranch: "main", HeadBranch: "feature/fails", MergeCommitSHA: "aaa",
	}}}
	syncer := &fakeMergedPRSync{
		refs:       map[string]domain.Ref{"feature/fails": {Target: target}},
		appendErrs: map[domain.ContentHash]error{target: errors.New("remote unavailable")},
	}

	if reflected := appendMergedPRContexts(
		context.Background(),
		resolver,
		syncer,
		"/repo",
		"main",
		"https://github.com/acme/project",
		[]string{"aaa"},
	); reflected {
		t.Fatal("failed PR promotion was reported as reflected")
	}
}

func TestIncomingCommitSHAsOldestFirstAndBounded(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	runGitForTest(t, repo, "init", "-b", "main")
	// Keep this repository hermetic. Developer/CI machines can have a global
	// cxt hooksPath; running 202 commits through those hooks performs real
	// checkpoint/network work and made this pure rev-list test flaky (#42).
	// Disable ambient auto-maintenance too: a background pack can race TempDir
	// cleanup after the 202-commit loop.
	runGitForTest(t, repo, "config", "core.hooksPath", t.TempDir())
	runGitForTest(t, repo, "config", "gc.auto", "0")
	runGitForTest(t, repo, "config", "maintenance.auto", "false")
	runGitForTest(t, repo, "config", "user.name", "Test")
	runGitForTest(t, repo, "config", "user.email", "test@example.com")
	for i := 0; i < 202; i++ {
		runGitForTest(t, repo, "commit", "--allow-empty", "-m", "commit")
	}
	all := strings.Fields(gitOut(repo, "rev-list", "--reverse", "HEAD"))
	if len(all) != 202 {
		t.Fatalf("commits = %d, want 202", len(all))
	}
	runGitForTest(t, repo, "update-ref", "ORIG_HEAD", all[0])

	got := incomingCommitSHAs(repo)
	if len(got) != 200 {
		t.Fatalf("incoming SHAs = %d, want bounded 200", len(got))
	}
	// ORIG_HEAD is excluded, leaving 201 commits; the oldest one is dropped by
	// the 200-entry defense.
	if got[0] != all[2] || got[len(got)-1] != all[len(all)-1] {
		t.Fatalf("bounded order = %s…%s, want %s…%s", got[0], got[len(got)-1], all[2], all[len(all)-1])
	}
}

func runGitForTest(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", filepath.Clean(cwd)}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
