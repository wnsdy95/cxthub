package cli

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func retryDueMergeDelivery(saved mergeDelivery, now time.Time) bool {
	if saved.Completed || saved.LeaseUntil.After(now) {
		return false
	}
	if saved.LastAttempt.IsZero() {
		return true
	}
	delay := min(time.Second<<min(max(saved.Attempts-1, 0), 6), time.Minute)
	return now.Before(saved.LastAttempt) || !now.Before(saved.LastAttempt.Add(delay))
}

func automaticMergeDeliveryKeys(jobs map[string]mergeDelivery, scope mergeEvidence, order map[string]int, now time.Time) []string {
	var keys []string
	for key, saved := range jobs {
		if saved.Completed || !sameMergeScope(saved.Evidence, scope) {
			continue
		}
		if saved.LeaseUntil.After(now) {
			return nil
		}
		if _, ancestral := order[saved.Evidence.After]; ancestral {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(left, right int) bool {
		first, second := jobs[keys[left]], jobs[keys[right]]
		if order[first.Evidence.After] != order[second.Evidence.After] {
			return order[first.Evidence.After] > order[second.Evidence.After]
		}
		if !first.CreatedAt.Equal(second.CreatedAt) {
			return first.CreatedAt.Before(second.CreatedAt)
		}
		return keys[left] < keys[right]
	})
	var due []string
	for _, key := range keys {
		saved := jobs[key]
		if retryDueMergeDelivery(saved, now) {
			due = append(due, key)
		}
		if !(saved.WaitingSource && len(saved.Candidates) == 0) && !saved.ReviewRequired {
			break
		}
	}
	sort.SliceStable(due, func(left, right int) bool {
		return jobs[due[left]].LastAttempt.Before(jobs[due[right]].LastAttempt)
	})
	return due
}

func discoveryRoot(ctx context.Context, cwd string) (string, fetchBinding, error) {
	root, binding, err := readFetchBinding(ctx, cwd)
	if err != nil || root != "" {
		return root, binding, err
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if state.GitRepository && state.Initialized {
		root = state.Root
	}
	return root, binding, nil
}

func mergeDeliveryAncestry(ctx context.Context, cwd string) (map[string]int, error) {
	raw, err := fetchGit(ctx, cwd, "rev-list", "--topo-order", "HEAD")
	if err != nil {
		return nil, err
	}
	ancestry := map[string]int{}
	for index, commit := range strings.Fields(raw) {
		ancestry[commit] = index
	}
	return ancestry, nil
}

func pendingMergeDeliveryWake(ctx context.Context, cwd, root string, now time.Time) (bool, error) {
	jobs, err := listMergeDeliveries(root)
	if err != nil || len(jobs) == 0 {
		return false, err
	}
	due := false
	for _, saved := range jobs {
		due = due || retryDueMergeDelivery(saved, now)
	}
	if !due {
		return false, nil
	}
	branch, err := fetchGit(ctx, cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch == "" {
		return false, nil
	}
	destination, err := mergeDeliveryDestination(root)
	if err != nil {
		return false, err
	}
	origin, err := fetchGit(ctx, cwd, "config", "--get", "remote.origin.url")
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return false, err
	}
	ancestry, err := mergeDeliveryAncestry(ctx, cwd)
	if err != nil {
		return false, err
	}
	remote, originHash := mergeDestinationHash(destination), domain.HashContent([]byte(origin))
	for _, saved := range jobs {
		_, ancestral := ancestry[saved.Evidence.After]
		if retryDueMergeDelivery(saved, now) && saved.Evidence.LocalBranch == branch && saved.Evidence.Remote == remote && saved.Evidence.Origin == originHash && ancestral {
			return true, nil
		}
	}
	return false, nil
}

func replayAutomaticMergeDeliveries(ctx context.Context, c *Container, cwd string) (bool, bool, error) {
	root := cxtRepoRoot(ctx, cwd)
	before, err := listMergeDeliveries(root)
	if err != nil || len(before) == 0 {
		return false, false, err
	}
	unfinished := false
	for _, saved := range before {
		unfinished = unfinished || !saved.Completed
	}
	if !unfinished {
		return false, false, nil
	}
	scope, err := mergeDeliveryScope(ctx, c, cwd)
	if err != nil {
		return false, false, err
	}
	ancestry, err := mergeDeliveryAncestry(ctx, cwd)
	if err != nil {
		return false, false, err
	}
	pending := false
	for _, saved := range before {
		_, ancestral := ancestry[saved.Evidence.After]
		if !saved.Completed && sameMergeScope(saved.Evidence, scope) && ancestral {
			pending = true
		}
	}
	keys := automaticMergeDeliveryKeys(before, scope, ancestry, time.Now())
	if len(keys) == 0 {
		return pending, false, nil
	}
	baseline, err := localBranchTarget(ctx, c, cwd, scope.LocalBranch)
	if err != nil {
		return true, false, err
	}
	if err := verifyMergeDeliveryScope(ctx, c, cwd, before[keys[0]].Evidence); err != nil {
		return false, false, err
	}
	if err := preservePullBriefingBaseline(cwd, scope.LocalBranch, baseline); err != nil {
		return true, false, err
	}
	out, reflected, _ := replayMergeDeliveriesAt(ctx, c, cwd, true)
	if reflected {
		writePullBriefingFromBaseline(ctx, c, cwd, scope.LocalBranch, baseline, out.requireBranchPlan)
	}
	after, err := listMergeDeliveries(root)
	if err != nil {
		return true, false, err
	}
	pending, progressed := false, false
	for key, saved := range after {
		_, ancestral := ancestry[saved.Evidence.After]
		if !sameMergeScope(saved.Evidence, scope) || !ancestral {
			continue
		}
		pending = pending || !saved.Completed
		previous, existed := before[key]
		if existed && ((!previous.Completed && saved.Completed) || previous.PlanHash != saved.PlanHash) {
			progressed = true
		}
	}
	return pending, progressed, ctx.Err()
}
