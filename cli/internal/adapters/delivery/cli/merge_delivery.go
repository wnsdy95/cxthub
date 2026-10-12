package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

type mergeEvidence struct {
	Version     int                `json:"version"`
	RepoID      string             `json:"repo_id"`
	Remote      domain.ContentHash `json:"remote_fingerprint"`
	Origin      domain.ContentHash `json:"origin_fingerprint"`
	LocalBranch string             `json:"local_branch"`
	Branch      string             `json:"branch"`
	BranchID    string             `json:"branch_id"`
	Before      string             `json:"before"`
	After       string             `json:"after"`
	SHAs        []string           `json:"shas"`
	Rewrites    map[string]string  `json:"rewrites,omitempty"`
}

type mergeDelivery struct {
	Evidence          mergeEvidence        `json:"evidence"`
	EvidenceHash      domain.ContentHash   `json:"evidence_hash"`
	CreatedAt         time.Time            `json:"created_at"`
	LastAttempt       time.Time            `json:"last_attempt,omitempty"`
	Attempts          int                  `json:"attempts"`
	Lease             string               `json:"lease,omitempty"`
	LeaseUntil        time.Time            `json:"lease_until,omitempty"`
	RequireBranchPlan bool                 `json:"require_branch_plan,omitempty"`
	Candidates        []domain.ContentHash `json:"candidates,omitempty"`
	PlanHash          domain.ContentHash   `json:"plan_hash,omitempty"`
	Completed         bool                 `json:"completed"`
	Reason            string               `json:"reason,omitempty"`
	WaitingSource     bool                 `json:"waiting_source,omitempty"`
	LateSource        bool                 `json:"late_source,omitempty"`
	ReviewRequired    bool                 `json:"review_required,omitempty"`
}

var errMergeSourcePending = errors.New("source context not yet available")
var errMergeHistoricalInsertion = errors.New("historical insertion requires review: a later merge is already complete")

func mergeDeliveryKey(evidence mergeEvidence) string {
	evidence.SHAs, evidence.Rewrites = nil, nil
	raw, _ := json.Marshal(evidence)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func mergeEvidenceHash(evidence mergeEvidence) domain.ContentHash {
	raw, _ := json.Marshal(evidence)
	return domain.HashContent(raw)
}

func mergeDeliveryPath(key string) string {
	return filepath.Join(".cxt", "merge-deliveries", key+".json")
}

func mergePlanHash(key string, candidates []domain.ContentHash, lateSource bool) domain.ContentHash {
	raw, _ := json.Marshal([]any{key, candidates, lateSource})
	return domain.HashContent(raw)
}

func readMergeDelivery(root, key string) (mergeDelivery, error) {
	raw, err := providerfs.ReadRepoFile(root, mergeDeliveryPath(key))
	if err != nil {
		return mergeDelivery{}, err
	}
	var job mergeDelivery
	if json.Unmarshal(raw, &job) != nil || job.Evidence.Version != 1 || mergeDeliveryKey(job.Evidence) != key || job.EvidenceHash != mergeEvidenceHash(job.Evidence) || job.CreatedAt.IsZero() || len(job.Evidence.SHAs) == 0 {
		return job, domain.ErrHashMismatch
	}
	if domain.ValidateContentHash(domain.ContentHash(job.Evidence.RepoID)) != nil || domain.ValidateBranchName(job.Evidence.Branch) != nil || job.Evidence.BranchID == "" || !domain.ValidGitOID(job.Evidence.Before) || !domain.ValidGitOID(job.Evidence.After) {
		return job, domain.ErrHashMismatch
	}
	for _, sha := range job.Evidence.SHAs {
		if !domain.ValidGitOID(sha) {
			return job, domain.ErrHashMismatch
		}
	}
	for _, candidate := range job.Candidates {
		if domain.ValidateContentHash(candidate) != nil {
			return job, domain.ErrHashMismatch
		}
	}
	if (len(job.Candidates) > 0 && job.PlanHash != mergePlanHash(key, job.Candidates, job.LateSource)) || (job.Completed && len(job.Candidates) == 0) {
		return job, domain.ErrHashMismatch
	}
	return job, nil
}

func writeMergeDelivery(root, key string, job mergeDelivery) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileDurable(root, mergeDeliveryPath(key), raw, 0600)
}

func mergeDeliveryScope(ctx context.Context, c *Container, cwd string) (mergeEvidence, error) {
	if c.ResolveRepo == nil || c.History == nil {
		return mergeEvidence{}, fmt.Errorf("merge delivery identity services unavailable")
	}
	repo, err := c.ResolveRepo(ctx, cwd)
	if err != nil {
		return mergeEvidence{}, err
	}
	local := gitOut(cwd, "symbolic-ref", "--short", "HEAD")
	if local == "" {
		return mergeEvidence{}, fmt.Errorf("merge delivery requires a selected Git branch")
	}
	binding, err := c.History.ResolveLocalBranch(ctx, string(repo.ID), local)
	if err != nil {
		return mergeEvidence{}, err
	}
	if binding.Inactive || binding.BranchID == "" || binding.Branch == "" {
		return mergeEvidence{}, domain.ErrSyncConflict
	}
	destination, err := mergeDeliveryDestination(cxtRepoRoot(ctx, cwd))
	if err != nil {
		return mergeEvidence{}, err
	}
	return mergeEvidence{Version: 1, RepoID: string(repo.ID), Remote: mergeDestinationHash(destination), Origin: domain.HashContent([]byte(gitOut(cwd, "config", "--get", "remote.origin.url"))), LocalBranch: local, Branch: binding.Branch, BranchID: binding.BranchID}, nil
}

func mergeDeliveryDestination(root string) (CapturePushDestination, error) {
	remotes, err := remotecfg.Load(root)
	if err != nil {
		return CapturePushDestination{}, err
	}
	destination := CapturePushDestination{URL: remotes["origin"]}
	if destination.URL == "" {
		destination.URL, destination.APIOnly = os.Getenv("CXT_REMOTE"), true
	}
	if destination.URL == "" {
		return destination, fmt.Errorf("context origin unavailable")
	}
	return destination, nil
}

func mergeDestinationHash(destination CapturePushDestination) domain.ContentHash {
	raw, _ := json.Marshal(destination)
	return domain.HashContent(raw)
}

func mergeScopeKey(evidence mergeEvidence) string {
	evidence.Before, evidence.After = "", ""
	return mergeDeliveryKey(evidence)
}

func sameMergeScope(saved, current mergeEvidence) bool {
	return saved.RepoID == current.RepoID && saved.Remote == current.Remote && saved.Origin == current.Origin && saved.LocalBranch == current.LocalBranch && saved.Branch == current.Branch && saved.BranchID == current.BranchID
}

func persistMergeDelivery(ctx context.Context, c *Container, cwd string, incoming incomingGitRange) error {
	if len(incoming.SHAs) == 0 {
		return nil
	}
	evidence, err := mergeDeliveryScope(ctx, c, cwd)
	if err != nil {
		return err
	}
	evidence.Before, evidence.After = incoming.Before, incoming.After
	if !domain.ValidGitOID(evidence.Before) || !domain.ValidGitOID(evidence.After) {
		return domain.ErrHashMismatch
	}
	root := cxtRepoRoot(ctx, cwd)
	evidence.SHAs = incoming.SHAs
	key := mergeDeliveryKey(evidence)
	_, err = providerfs.WithCxtLock(ctx, root, "merge-delivery-branches", mergeScopeKey(evidence), syscall.LOCK_EX, true, func() error {
		_, err := readMergeDelivery(root, key)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		raw, err := providerfs.ReadRepoFile(root, filepath.Join(".cxt", "rewrites.json"))
		if err == nil {
			if json.Unmarshal(raw, &evidence.Rewrites) != nil {
				return domain.ErrHashMismatch
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		return writeMergeDelivery(root, key, mergeDelivery{Evidence: evidence, EvidenceHash: mergeEvidenceHash(evidence), CreatedAt: time.Now().UTC()})
	})
	return err
}

func listMergeDeliveries(root string) (map[string]mergeDelivery, error) {
	probe := filepath.Join(root, ".cxt", "merge-deliveries", "probe")
	if err := providerfs.ValidateCxtWritePath(probe); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(probe))
	if os.IsNotExist(err) {
		return map[string]mergeDelivery{}, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := map[string]mergeDelivery{}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key := entry.Name()[:len(entry.Name())-5]
		job, err := readMergeDelivery(root, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("merge delivery %s: %w", key, err))
			continue
		}
		jobs[key] = job
	}
	return jobs, errors.Join(failures...)
}

func updateLeasedMerge(ctx context.Context, root, key, lease string, update func(*mergeDelivery)) error {
	_, err := providerfs.WithCxtLock(ctx, root, "merge-deliveries", key, syscall.LOCK_EX, true, func() error {
		job, err := readMergeDelivery(root, key)
		if err != nil {
			return err
		}
		if job.Lease != lease || !job.LeaseUntil.After(time.Now()) {
			return domain.ErrSyncConflict
		}
		update(&job)
		return writeMergeDelivery(root, key, job)
	})
	return err
}

func verifyMergeDeliveryScope(ctx context.Context, c *Container, cwd string, evidence mergeEvidence) error {
	current, err := mergeDeliveryScope(ctx, c, cwd)
	if err != nil {
		return err
	}
	if !sameMergeScope(evidence, current) {
		return domain.ErrSyncConflict
	}
	if err := exec.CommandContext(ctx, "git", "-C", cwd, "merge-base", "--is-ancestor", evidence.After, "HEAD").Run(); err != nil {
		return fmt.Errorf("recorded merge no longer belongs to selected code: %w", err)
	}
	return nil
}

func executeMergeDelivery(ctx context.Context, c *Container, cwd, key, lease string, job mergeDelivery) (incomingContextFetch, bool, error) {
	evidence := job.Evidence
	root := cxtRepoRoot(ctx, cwd)
	if err := verifyMergeDeliveryScope(ctx, c, cwd, evidence); err != nil {
		return incomingContextFetch{}, false, err
	}
	if c.PrepareCapturePush != nil {
		destination, err := mergeDeliveryDestination(root)
		if err != nil {
			return incomingContextFetch{}, false, err
		}
		if mergeDestinationHash(destination) != evidence.Remote {
			return incomingContextFetch{}, false, domain.ErrSyncConflict
		}
		syncer, err := c.PrepareCapturePush(ctx, cwd, destination, evidence.RepoID)
		if err != nil {
			return incomingContextFetch{}, false, err
		}
		bound := *c
		bound.Sync = syncer
		c = &bound
	}
	out := incomingContextFetch{requireBranchPlan: job.RequireBranchPlan}
	if len(job.Candidates) == 0 {
		var err error
		out, err = fetchIncomingContextsWithRewrites(ctx, c, cwd, evidence.Branch, evidence.SHAs, evidence.Rewrites, job.RequireBranchPlan, func() error {
			if job.RequireBranchPlan {
				return nil
			}
			job.RequireBranchPlan = true
			return updateLeasedMerge(ctx, root, key, lease, func(saved *mergeDelivery) { saved.RequireBranchPlan = true })
		})
		if err != nil {
			return out, false, err
		}
		listing, err := c.List.List(ctx, inbound.ListInput{})
		if err != nil {
			return out, false, err
		}
		for _, candidate := range mergedContextCandidates(listing.Snapshots, evidence.Rewrites, evidence.SHAs) {
			job.Candidates = append(job.Candidates, candidate.ID)
		}
		if len(job.Candidates) == 0 {
			return out, false, errMergeSourcePending
		}
		job.LateSource = job.WaitingSource
		job.PlanHash = mergePlanHash(key, job.Candidates, job.LateSource)
		if err := updateLeasedMerge(ctx, root, key, lease, func(saved *mergeDelivery) {
			saved.Candidates, saved.PlanHash, saved.LateSource = job.Candidates, job.PlanHash, job.LateSource
			saved.WaitingSource = false
		}); err != nil {
			return out, false, err
		}
	}
	if err := verifyMergeDeliveryScope(ctx, c, cwd, evidence); err != nil {
		return out, false, err
	}
	coverage, current, err := mergeContextCoverage(ctx, c, cwd, evidence, job.Candidates)
	if err != nil {
		return out, false, err
	}
	candidates := make([]domain.Snapshot, 0, len(job.Candidates))
	for _, candidate := range job.Candidates {
		if !coverage[candidate] {
			candidates = append(candidates, domain.Snapshot{ID: candidate})
		}
	}
	if len(candidates) == 0 {
		return out, current, nil
	}
	in := inbound.SyncInput{Cwd: cwd, RepoID: evidence.RepoID, RequireBranchPlan: job.RequireBranchPlan, ExpectedBranchID: evidence.BranchID}
	reflected, err := appendMergeCandidates(ctx, c, evidence.Branch, candidates, in, evidence.After, func() error {
		if err := verifyMergeDeliveryScope(ctx, c, cwd, evidence); err != nil {
			return err
		}
		saved, err := readMergeDelivery(root, key)
		if err != nil {
			return err
		}
		if saved.Lease != lease || !saved.LeaseUntil.After(time.Now()) {
			return domain.ErrSyncConflict
		}
		jobs, err := listMergeDeliveries(root)
		if err != nil {
			return err
		}
		for _, later := range jobs {
			if later.Completed && sameMergeScope(evidence, later.Evidence) && later.Evidence.After != evidence.After {
				err := exec.CommandContext(ctx, "git", "-C", cwd, "merge-base", "--is-ancestor", evidence.After, later.Evidence.After).Run()
				if err == nil {
					return errMergeHistoricalInsertion
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					return fmt.Errorf("cannot verify completed merge order: %w", err)
				}
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return out, reflected, err
	}
	observer, ok := c.Sync.(inbound.RemoteBranchObserver)
	if !ok {
		return out, reflected, fmt.Errorf("remote observation unavailable")
	}
	observed, err := observer.ResolveRemoteBranchObservation(ctx, in, evidence.Branch)
	if err != nil {
		return out, reflected, err
	}
	if observedBranchIdentity(observed.Ref) != evidence.BranchID || observed.Ref.RepoID != evidence.RepoID {
		return out, reflected, domain.ErrSyncConflict
	}
	coverage, _, err = mergeContextCoverage(ctx, c, cwd, evidence, job.Candidates)
	if err != nil {
		return out, reflected, err
	}
	_, contextSupported := c.Sync.(inbound.RemoteBranchContextReader)
	for _, candidate := range job.Candidates {
		if !coverage[candidate] && (contextSupported || !snapshotContains(observed.Snapshots, observed.Ref.Target, candidate)) {
			return out, reflected, fmt.Errorf("server inclusion not yet verified for %s", candidate)
		}
	}
	return out, reflected, nil
}

func replayMergeDeliveries(ctx context.Context, c *Container, cwd string) (incomingContextFetch, bool, bool) {
	return replayMergeDeliveriesAt(ctx, c, cwd, false)
}

func replayMergeDeliveriesAt(ctx context.Context, c *Container, cwd string, automatic bool) (incomingContextFetch, bool, bool) {
	root := cxtRepoRoot(ctx, cwd)
	if _, err := os.Stat(filepath.Join(root, ".cxt", "merge-deliveries")); os.IsNotExist(err) {
		return incomingContextFetch{}, false, false
	}
	scope, err := mergeDeliveryScope(ctx, c, cwd)
	if err != nil {
		hookWarn("merge delivery scope: %v", err)
		return incomingContextFetch{}, false, false
	}
	key, lease := "", domain.NewSessionID()
	var job mergeDelivery
	_, err = providerfs.WithCxtLock(ctx, root, "merge-delivery-branches", mergeScopeKey(scope), syscall.LOCK_EX, true, func() error {
		jobs, err := listMergeDeliveries(root)
		if err != nil {
			return err
		}
		var keys []string
		order := map[string]int{}
		raw, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-list", "--topo-order", "HEAD").Output()
		if err != nil {
			return err
		}
		for index, sha := range strings.Fields(string(raw)) {
			order[sha] = index
		}
		if automatic {
			keys = automaticMergeDeliveryKeys(jobs, scope, order, time.Now())
		} else {
			for candidate, saved := range jobs {
				if saved.Completed || !sameMergeScope(saved.Evidence, scope) {
					continue
				}
				if saved.LeaseUntil.After(time.Now()) {
					return nil
				}
				if _, ancestral := order[saved.Evidence.After]; !ancestral {
					continue
				}
				keys = append(keys, candidate)
			}
			sort.Slice(keys, func(left, right int) bool {
				first, second := jobs[keys[left]], jobs[keys[right]]
				firstDeferred := (first.WaitingSource && len(first.Candidates) == 0) || first.ReviewRequired
				secondDeferred := (second.WaitingSource && len(second.Candidates) == 0) || second.ReviewRequired
				if firstDeferred != secondDeferred {
					return !firstDeferred
				}
				if firstDeferred && !first.LastAttempt.Equal(second.LastAttempt) {
					return first.LastAttempt.Before(second.LastAttempt)
				}
				firstOrder, secondOrder := order[first.Evidence.After], order[second.Evidence.After]
				if firstOrder != secondOrder {
					return firstOrder > secondOrder
				}
				if !first.CreatedAt.Equal(second.CreatedAt) {
					return first.CreatedAt.Before(second.CreatedAt)
				}
				return keys[left] < keys[right]
			})
		}
		if len(keys) == 0 {
			return nil
		}
		key, job = keys[0], jobs[keys[0]]
		_, err = providerfs.WithCxtLock(ctx, root, "merge-deliveries", key, syscall.LOCK_EX, true, func() error {
			job, err = readMergeDelivery(root, key)
			if err != nil {
				return err
			}
			if job.Completed || job.LeaseUntil.After(time.Now()) {
				key = ""
				return nil
			}
			job.Lease, job.LeaseUntil = lease, time.Now().UTC().Add(65*time.Second)
			job.LastAttempt, job.Attempts = time.Now().UTC(), job.Attempts+1
			return writeMergeDelivery(root, key, job)
		})
		return err
	})
	if err != nil {
		hookWarn("merge delivery claim: %v", err)
		return incomingContextFetch{}, false, false
	}
	if key == "" {
		return incomingContextFetch{}, false, false
	}
	attemptCtx, cancel := context.WithTimeout(ctx, time.Minute)
	out, reflected, attemptErr := executeMergeDelivery(attemptCtx, c, cwd, key, lease, job)
	cancel()
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	err = updateLeasedMerge(finishCtx, root, key, lease, func(saved *mergeDelivery) {
		saved.Completed = attemptErr == nil
		saved.WaitingSource = len(saved.Candidates) == 0 && (saved.WaitingSource || errors.Is(attemptErr, errMergeSourcePending))
		saved.ReviewRequired = !saved.Completed && (saved.ReviewRequired || errors.Is(attemptErr, errMergeHistoricalInsertion))
		saved.Lease, saved.LeaseUntil = "", time.Time{}
		saved.Reason = ""
		if attemptErr != nil {
			saved.Reason = attemptErr.Error()
		}
	})
	finishCancel()
	if err != nil {
		hookWarn("merge delivery acknowledgement: %v", err)
	}
	if attemptErr != nil {
		hookWarn("merge delivery %s remains pending: %v", key[:12], attemptErr)
	}
	return out, reflected, true
}

func replaySavedMergeDeliveries(ctx context.Context, c *Container, cwd string) {
	if c.ResolveRepo == nil || c.History == nil {
		return
	}
	branch := gitOut(cwd, "symbolic-ref", "--short", "HEAD")
	baseline, err := localBranchTarget(ctx, c, cwd, branch)
	if err != nil {
		hookWarn("merge delivery baseline: %v", err)
		return
	}
	if err := preservePullBriefingBaseline(cwd, branch, baseline); err != nil {
		hookWarn("merge delivery baseline: %v", err)
		return
	}
	out, reflected, _ := replayMergeDeliveries(ctx, c, cwd)
	if reflected {
		writePullBriefingFromBaseline(ctx, c, cwd, branch, baseline, out.requireBranchPlan)
	}
}
