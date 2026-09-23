package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"path/filepath"
	"testing"
)

type fairnessPRResolver struct{ calls []string }

func (r *fairnessPRResolver) ResolveMergedPullRequests(_ context.Context, _, base string, shas []string) ([]outbound.MergedPullRequest, error) {
	r.calls = append(r.calls, shas...)
	n := 245
	if shas[0] == "older-merge" {
		n = 243
	}
	return []outbound.MergedPullRequest{{Number: n, BaseBranch: base, HeadBranch: "feature", HeadSHA: "head", MergeCommitSHA: shas[0]}}, nil
}

type fairnessPRPromoter struct {
	fakeMergedPRSync
	calls []int
}

func (s *fairnessPRPromoter) PromotePullRequest(_ context.Context, _ inbound.SyncInput, pr outbound.MergedPullRequest) error {
	s.calls = append(s.calls, pr.Number)
	if pr.Number == 243 {
		return errors.Join(domain.ErrSyncConflict, errors.New("source_finalization_required"))
	}
	return nil
}
func TestPendingPRDoesNotStarveLaterDiscovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "https://github.com/review/repo"
	resolver := &fairnessPRResolver{}
	syncer := &fairnessPRPromoter{}
	if err := persistPRDiscovery(ctx, root, "main", origin, []string{"older-merge"}); err != nil {
		t.Fatal(err)
	}
	if err := persistPRDiscovery(ctx, root, "main", origin, []string{"later-merge"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		replayPRDiscovery(ctx, resolver, syncer, root, "main", origin, nil)
	}
	t.Logf("three sync attempts: resolver=%v submitted PRs=%v", resolver.calls, syncer.calls)
	for _, n := range syncer.calls {
		if n == 245 {
			return
		}
	}
	t.Fatal("later merged PR was never discovered or submitted while the older PR needs attention")
}

type failingRangeResolver struct{ calls map[string]bool }

func (r *failingRangeResolver) ResolveMergedPullRequests(_ context.Context, _, _ string, shas []string) ([]outbound.MergedPullRequest, error) {
	r.calls[shas[0]] = true
	return nil, errors.New("provider offline")
}
func TestPRDiscoveryRetryBudgetRotatesAndSkipsCorruptRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "https://github.com/review/repo"
	for i := 0; i < 7; i++ {
		if err := persistPRDiscovery(ctx, root, "main", origin, []string{fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := filepath.Join(root, ".cxt", "pr-discovery", "broken.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := &failingRangeResolver{calls: map[string]bool{}}
	for i := 0; i < 2; i++ {
		replayPRDiscovery(ctx, resolver, &fairnessPRPromoter{}, root, "main", origin, nil)
	}
	if len(resolver.calls) != 7 {
		t.Fatalf("starved ranges: %v", resolver.calls)
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatal("corrupt evidence was deleted")
	}
}

type queuedFailingSync struct {
	fixedBriefingSync
	queued []int
}

func (s *queuedFailingSync) QueuePullRequest(_ context.Context, _ inbound.SyncInput, pr outbound.MergedPullRequest) error {
	s.queued = append(s.queued, pr.Number)
	return nil
}
func (*queuedFailingSync) Pull(context.Context, inbound.SyncInput) (inbound.SyncOutput, error) {
	return inbound.SyncOutput{}, errors.New("object fetch failed")
}
func (*queuedFailingSync) Push(context.Context, inbound.SyncInput) (inbound.SyncOutput, error) {
	return inbound.SyncOutput{}, errors.New("object push failed")
}
func TestDiscoveryHandoffPrecedesUnrelatedObjectFailure(t *testing.T) {
	for _, command := range []string{"push", "pull", "post-merge"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			runGitForTest(t, root, "init", "-q", "-b", "main")
			for _, kv := range [][2]string{{"core.hooksPath", "/dev/null"}, {"commit.gpgsign", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}, {"user.name", "Test"}, {"user.email", "test@example.test"}} {
				runGitForTest(t, root, "config", kv[0], kv[1])
			}
			origin := "https://github.com/acme/repo.git"
			runGitForTest(t, root, "remote", "add", "origin", origin)
			runGitForTest(t, root, "commit", "--allow-empty", "-qm", "base")
			if err := persistPRDiscovery(context.Background(), root, "main", origin, []string{"merge"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CXT_REMOTE", "http://example.test")
			t.Chdir(root)
			syncer := &queuedFailingSync{}
			c := &Container{Sync: syncer, PRMerges: &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 42, BaseBranch: "main", HeadBranch: "feature"}}}}
			if command == "post-merge" {
				handleIncomingContexts(context.Background(), c, root)
			} else if err := Run(c, []string{"cxt", command}); err == nil {
				t.Fatal("object failure hidden")
			}
			if len(syncer.queued) != 1 || syncer.queued[0] != 42 {
				t.Fatalf("object failure blocked durable PR handoff: %v", syncer.queued)
			}
		})
	}
}
