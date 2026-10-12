package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

const fetchObservationDir = ".cxt/fetch-observations"

type fetchRemote struct {
	Name  string   `json:"name"`
	URLs  []string `json:"urls"`
	Fetch []string `json:"fetch"`
}

type fetchBinding struct {
	Origin            string        `json:"origin"`
	GitRepository     string        `json:"git_repository"`
	ContextOrigin     string        `json:"context_origin"`
	ContextRepository string        `json:"context_repository"`
	ConfigIdentity    string        `json:"config_identity"`
	Remotes           []fetchRemote `json:"remotes"`
}

type fetchEvidence struct {
	fetchBinding
	Before string `json:"before"`
	After  string `json:"after"`
	Ref    string `json:"ref"`
}

type fetchObservation struct {
	fetchEvidence
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	Cursor      int       `json:"cursor"`
	Scanned     bool      `json:"discovery_scanned"`
	Delivery    string    `json:"delivery_state,omitempty"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
}

func fetchIdentity(value any) string {
	raw, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func fetchGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1")
	raw, err := cmd.Output()
	return strings.TrimSpace(string(raw)), err
}

func readFetchBinding(ctx context.Context, cwd string) (string, fetchBinding, error) {
	var binding fetchBinding
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !state.GitRepository || !state.Initialized {
		return "", binding, nil
	}
	remotes, err := remotecfg.Load(state.Root)
	if err != nil || remotes["origin"] == "" {
		return "", binding, err
	}
	binding.ContextOrigin = remotes["origin"]
	binding.ContextRepository = string(remotecfg.RepoIDFor(binding.ContextOrigin))
	raw, err := providerfs.ReadRepoFile(state.Root, ".cxt/config")
	if err != nil {
		return "", binding, err
	}
	binding.ConfigIdentity = fetchIdentity(raw)
	var config struct {
		Remotes remotecfg.Remotes `json:"remotes"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return "", binding, domain.ErrHashMismatch
	}
	origin, err := remotecfg.CanonicalURL(config.Remotes["origin"])
	if err != nil || origin != binding.ContextOrigin {
		return "", binding, errors.Join(domain.ErrSyncConflict, err)
	}
	names, err := fetchGit(ctx, cwd, "remote")
	if err != nil {
		return "", binding, err
	}
	for _, name := range strings.Fields(names) {
		urls, err := fetchGit(ctx, cwd, "config", "--get-all", "remote."+name+".url")
		if err != nil {
			return "", binding, err
		}
		specs, err := fetchGit(ctx, cwd, "config", "--get-all", "remote."+name+".fetch")
		var exitErr *exec.ExitError
		if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
			return "", binding, err
		}
		remote := fetchRemote{Name: name}
		for _, rawURL := range strings.Split(urls, "\n") {
			url := gitctx.SanitizeRemoteURL(rawURL)
			if url == "" {
				url = "unknown:" + fetchIdentity(rawURL)
			}
			remote.URLs = append(remote.URLs, url)
		}
		if specs != "" {
			remote.Fetch = strings.Split(specs, "\n")
		}
		binding.Remotes = append(binding.Remotes, remote)
		if name == "origin" && len(remote.URLs) == 1 {
			binding.Origin = remote.URLs[0]
			binding.GitRepository = gitctx.NormalizeRemoteURL(binding.Origin)
		}
	}
	return state.Root, binding, nil
}

func (e fetchEvidence) branch() string {
	if !strings.HasPrefix(e.GitRepository, "github.com/") || len(strings.Split(e.GitRepository, "/")) != 3 ||
		!strings.HasPrefix(e.Ref, "refs/remotes/origin/") {
		return ""
	}
	originFound := false
	for _, remote := range e.Remotes {
		if remote.Name == "origin" {
			if len(remote.URLs) != 1 || len(remote.Fetch) != 1 || strings.TrimPrefix(remote.Fetch[0], "+") != "refs/heads/*:refs/remotes/origin/*" {
				return ""
			}
			originFound = true
			continue
		}
		if len(remote.Fetch) == 0 {
			return ""
		}
		for _, spec := range remote.Fetch {
			if !fetchDestinationDisjoint(spec, e.Ref) {
				return ""
			}
		}
	}
	if !originFound {
		return ""
	}
	branch := strings.TrimPrefix(e.Ref, "refs/remotes/origin/")
	if branch == "HEAD" || domain.ValidateBranchName(branch) != nil {
		return ""
	}
	return branch
}

func fetchDestinationDisjoint(spec, ref string) bool {
	validPattern := func(value string) bool {
		return strings.HasPrefix(value, "refs/") && strings.Count(value, "*") <= 1 &&
			domain.ValidateBranchName(strings.ReplaceAll(value, "*", "wildcard")) == nil
	}
	if strings.HasPrefix(spec, "^") {
		return validPattern(strings.TrimPrefix(spec, "^"))
	}
	parts := strings.Split(strings.TrimPrefix(spec, "+"), ":")
	if len(parts) != 2 || !validPattern(parts[0]) || !validPattern(parts[1]) || strings.Count(parts[0], "*") != strings.Count(parts[1], "*") {
		return false
	}
	prefix, suffix, wildcard := strings.Cut(parts[1], "*")
	if !wildcard {
		return parts[1] != ref
	}
	return !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) || len(ref) < len(prefix)+len(suffix)
}

func (e fetchEvidence) authorizes(current fetchBinding) bool {
	branch := e.branch()
	return branch != "" && current.GitRepository == e.GitRepository && current.ContextOrigin == e.ContextOrigin &&
		current.ContextRepository == e.ContextRepository && (fetchEvidence{fetchBinding: current, Ref: e.Ref}).branch() == branch
}

func observeFetchedRefs(ctx context.Context, cwd string, input io.Reader) error {
	root, binding, err := readFetchBinding(ctx, cwd)
	if err != nil || root == "" {
		return err
	}
	scanner := bufio.NewScanner(input)
	var failures error
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "refs/remotes/") {
			continue
		}
		if (!validNonZeroGitOID(fields[0]) && !zeroGitOID(fields[0])) || (!validNonZeroGitOID(fields[1]) && !zeroGitOID(fields[1])) {
			failures = errors.Join(failures, fmt.Errorf("remote ref transaction lacks object IDs"))
			continue
		}
		if fields[0] == fields[1] && !zeroGitOID(fields[0]) {
			continue
		}
		item := fetchObservation{
			fetchEvidence: fetchEvidence{fetchBinding: binding, Before: fields[0], After: fields[1], Ref: fields[2]},
			Version:       1, CreatedAt: time.Now().UTC(), Status: "unknown", Reason: "provider_history_unverified",
		}
		if item.branch() == "" {
			item.Reason = "unsupported_remote_scope"
		} else if zeroGitOID(item.After) {
			item.Reason = "deleted_ref"
		}
		key := fetchIdentity(item.fetchEvidence)
		file := filepath.Join(fetchObservationDir, key+".json")
		_, err := providerfs.WithCxtLock(ctx, root, "fetch-observations", key, syscall.LOCK_EX, true, func() error {
			if raw, err := providerfs.ReadRepoFile(root, file); err == nil {
				var saved fetchObservation
				if json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || fetchIdentity(saved.fetchEvidence) != key || saved.Cursor < 0 {
					return domain.ErrHashMismatch
				}
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			raw, _ := json.Marshal(item)
			return providerfs.WriteRepoFileDurable(root, file, raw, 0600)
		})
		failures = errors.Join(failures, err)
	}
	return errors.Join(failures, scanner.Err())
}

type fetchPRQueue interface {
	QueuePullRequest(context.Context, inbound.SyncInput, outbound.MergedPullRequest) error
}

type fetchQueueSync struct {
	fetchPRQueue
	cwd      string
	evidence fetchEvidence
}

func (s fetchQueueSync) QueuePullRequest(ctx context.Context, _ inbound.SyncInput, pr outbound.MergedPullRequest) error {
	root, current, err := readFetchBinding(ctx, s.cwd)
	if err != nil || root == "" || !s.evidence.authorizes(current) {
		return errors.Join(domain.ErrSyncConflict, err)
	}
	return s.fetchPRQueue.QueuePullRequest(ctx, inbound.SyncInput{Cwd: s.cwd, RepoID: s.evidence.ContextRepository}, pr)
}

func (fetchQueueSync) ResolveRemoteBranch(context.Context, inbound.SyncInput, string) (domain.Ref, error) {
	return domain.Ref{}, domain.ErrSyncConflict
}

func (fetchQueueSync) AppendBranch(context.Context, inbound.SyncInput, string, domain.ContentHash) error {
	return domain.ErrSyncConflict
}

func fetchScopedSync(ctx context.Context, c *Container, cwd string, evidence fetchEvidence) (inbound.SyncRepo, error) {
	root, current, err := readFetchBinding(ctx, cwd)
	if err != nil || root == "" || !evidence.authorizes(current) {
		return nil, errors.Join(domain.ErrSyncConflict, err)
	}
	syncer := c.Sync
	if c.ResolveSyncDestination != nil {
		destination, err := c.ResolveSyncDestination(ctx, cwd, "origin")
		if err != nil {
			return nil, err
		}
		syncer = destination.Sync
	}
	root, current, err = readFetchBinding(ctx, cwd)
	if err != nil || root == "" || !evidence.authorizes(current) {
		return nil, errors.Join(domain.ErrSyncConflict, err)
	}
	return syncer, nil
}

func replayFetchDeliveries(ctx context.Context, c *Container, cwd string, evidence fetchEvidence) (bool, error) {
	syncer, err := fetchScopedSync(ctx, c, cwd, evidence)
	if err != nil {
		return true, err
	}
	replayer, ok := syncer.(interface {
		ReplayPRDeliveries(context.Context, inbound.SyncInput) (bool, error)
	})
	if !ok {
		return true, fmt.Errorf("durable PR delivery replay unavailable")
	}
	return replayer.ReplayPRDeliveries(ctx, inbound.SyncInput{Cwd: cwd, RepoID: evidence.ContextRepository, Ref: evidence.branch()})
}

func replayFetchObservation(ctx context.Context, c *Container, cwd, root string, item *fetchObservation) (bool, error) {
	_, current, err := readFetchBinding(ctx, cwd)
	if err != nil || !item.fetchEvidence.authorizes(current) {
		item.Reason = "binding_changed"
		return false, errors.Join(domain.ErrSyncConflict, err)
	}
	shallow, err := fetchGit(ctx, cwd, "rev-parse", "--is-shallow-repository")
	if err != nil || shallow != "false" {
		item.Reason = "shallow_or_unavailable_history"
		return false, errors.Join(domain.ErrNotFound, err)
	}
	grafts, err := fetchGit(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(grafts); !os.IsNotExist(err) {
		item.Reason = "grafted_history_unsupported"
		return false, errors.Join(domain.ErrSyncConflict, err)
	}
	for _, oid := range []string{item.Before, item.After} {
		if zeroGitOID(oid) {
			continue
		}
		if _, err := fetchGit(ctx, cwd, "cat-file", "-e", oid+"^{commit}"); err != nil {
			item.Reason = "commit_unavailable"
			return false, err
		}
	}
	args := []string{"rev-list", "--topo-order", "--max-count=200", "--skip=" + strconv.Itoa(item.Cursor), item.After}
	if !zeroGitOID(item.Before) {
		args = append(args, "^"+item.Before)
	}
	raw, err := fetchGit(ctx, cwd, args...)
	if err != nil {
		item.Reason = "range_unavailable"
		return false, err
	}
	shas := strings.Fields(raw)
	if len(shas) == 0 {
		item.Scanned, item.Reason = true, "provider_history_unverified"
		return false, nil
	}
	slices.Reverse(shas)
	key, branch := fetchIdentity(item.fetchEvidence), item.branch()
	if err := persistPRDiscovery(ctx, cwd, branch, item.Origin, shas, key); err != nil {
		return false, err
	}
	syncer, err := fetchScopedSync(ctx, c, cwd, item.fetchEvidence)
	if err != nil {
		return false, err
	}
	queue, ok := syncer.(fetchPRQueue)
	if !ok || c.PRMerges == nil {
		return false, fmt.Errorf("durable PR discovery handoff unavailable")
	}
	_, progressed := replayPRDiscoveryWithProgress(ctx, c.PRMerges, fetchQueueSync{queue, cwd, item.fetchEvidence}, cwd, branch, item.Origin, nil, key)
	if _, err := providerfs.ReadRepoFile(root, prDiscoveryFile(item.Origin, branch, shas, key)); !os.IsNotExist(err) {
		if err == nil && progressed {
			return true, nil
		}
		return progressed, errors.Join(fmt.Errorf("PR discovery remains queued"), err)
	}
	item.Cursor += len(shas)
	item.Scanned = len(shas) < 200
	item.Reason = "provider_history_unverified"
	return progressed, nil
}

func replayFetchObservations(ctx context.Context, c *Container, cwd string) (bool, error) {
	pending, _, err := replayFetchObservationsWithProgress(ctx, c, cwd)
	return pending, err
}

func replayFetchObservationsWithProgress(ctx context.Context, c *Container, cwd string) (bool, bool, error) {
	root, _, err := readFetchBinding(ctx, cwd)
	if err != nil || root == "" {
		return false, false, err
	}
	if err := providerfs.ValidateCxtDir(filepath.Join(root, fetchObservationDir)); err != nil {
		return false, false, err
	}
	entries, err := os.ReadDir(filepath.Join(root, fetchObservationDir))
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	type pending struct {
		file string
		item fetchObservation
	}
	var work []pending
	var failures error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		file := filepath.Join(fetchObservationDir, entry.Name())
		raw, err := providerfs.ReadRepoFile(root, file)
		var item fetchObservation
		if err != nil || json.Unmarshal(raw, &item) != nil || item.Version != 1 || item.Cursor < 0 || fetchIdentity(item.fetchEvidence)+".json" != entry.Name() {
			failures = errors.Join(failures, fmt.Errorf("fetch observation %s is corrupt or unavailable", entry.Name()))
			continue
		}
		if (!item.Scanned || item.Delivery != "drained") && item.branch() != "" && !zeroGitOID(item.After) {
			work = append(work, pending{file, item})
		}
	}
	sort.Slice(work, func(left, right int) bool {
		if !work[left].item.LastAttempt.Equal(work[right].item.LastAttempt) {
			return work[left].item.LastAttempt.Before(work[right].item.LastAttempt)
		}
		return work[left].file < work[right].file
	})
	pendingWork := len(work) > 4
	progressed := false
	for _, next := range work[:min(4, len(work))] {
		if err := ctx.Err(); err != nil {
			return true, progressed, errors.Join(failures, err)
		}
		previous := next.item
		next.item.LastAttempt = time.Now().UTC()
		next.item.Reason = "discovery_or_handoff_pending"
		var attemptErr error
		if !next.item.Scanned {
			var discoveryProgress bool
			discoveryProgress, attemptErr = replayFetchObservation(ctx, c, cwd, root, &next.item)
			progressed = progressed || discoveryProgress
		}
		deliveryPending, deliveryErr := replayFetchDeliveries(ctx, c, cwd, next.item.fetchEvidence)
		next.item.Delivery = "drained"
		if deliveryPending || deliveryErr != nil {
			next.item.Delivery = "pending"
			if attemptErr == nil {
				next.item.Reason = "pr_delivery_pending"
				if errors.Is(deliveryErr, domain.ErrSyncConflict) {
					next.item.Reason = "binding_changed"
				}
			}
		} else if attemptErr == nil {
			next.item.Reason = "provider_history_unverified"
		}
		attemptErr = errors.Join(attemptErr, deliveryErr)
		pendingWork = pendingWork || !next.item.Scanned || next.item.Delivery != "drained"
		raw, _ := json.Marshal(next.item)
		if err := providerfs.WriteRepoFileDurable(root, next.file, raw, 0600); err != nil {
			return true, progressed, errors.Join(failures, attemptErr, err)
		}
		progressed = progressed || next.item.Cursor > previous.Cursor || (!previous.Scanned && next.item.Scanned) || (previous.Delivery == "pending" && next.item.Delivery == "drained")
		failures = errors.Join(failures, attemptErr)
	}
	return pendingWork, progressed, failures
}

func SpawnFetchDiscovery(cwd string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := spawnFetchDiscovery(ctx, cwd, time.Now(), func() error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "git-hook", "remote-ref-discovery")
		cmd.Dir = cwd
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	})
	if err != nil {
		hookWarn("fetch discovery wake failed: %v", err)
	}
}

const fetchLaunchLease = 10 * time.Second

func spawnFetchDiscovery(ctx context.Context, cwd string, now time.Time, start func() error) error {
	root, current, err := discoveryRoot(ctx, cwd)
	if err != nil || root == "" {
		return err
	}
	if err := providerfs.ValidateCxtDir(filepath.Join(root, fetchObservationDir)); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(root, fetchObservationDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pending := false
	var failures error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := providerfs.ReadRepoFile(root, filepath.Join(fetchObservationDir, entry.Name()))
		var item fetchObservation
		if err != nil || json.Unmarshal(raw, &item) != nil || item.Version != 1 || item.Cursor < 0 || fetchIdentity(item.fetchEvidence)+".json" != entry.Name() {
			failures = errors.Join(failures, fmt.Errorf("fetch observation %s is corrupt or unavailable", entry.Name()))
			continue
		}
		if (!item.Scanned || item.Delivery != "drained") && !zeroGitOID(item.After) && item.fetchEvidence.authorizes(current) {
			pending = true
			break
		}
	}
	mergePending, mergeErr := pendingMergeDeliveryWake(ctx, cwd, root, now)
	pending = pending || mergePending
	failures = errors.Join(failures, mergeErr)
	if !pending {
		return failures
	}
	if err := providerfs.WriteRepoFileDurable(root, filepath.Join(fetchObservationDir, "wake"), []byte(strconv.FormatInt(now.UnixNano(), 10)), 0600); err != nil {
		return err
	}
	lock, err := openFetchWorkerLock(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	leaseFile := filepath.Join(fetchObservationDir, "launch")
	raw, err := providerfs.ReadRepoFile(root, leaseFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if stamp, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		age := now.Sub(time.Unix(0, stamp))
		if age >= 0 && age < fetchLaunchLease {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := providerfs.WriteRepoFileDurable(root, leaseFile, []byte(strconv.FormatInt(now.UnixNano(), 10)), 0600); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		return err
	}
	return start()
}

func openFetchWorkerLock(root string) (*os.File, error) {
	path, err := providerfs.PrepareRepoFile(root, filepath.Join(fetchObservationDir, "worker.lock"), 0700)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		lock.Close()
		return nil, errors.Join(domain.ErrHashMismatch, err)
	}
	return lock, nil
}

func runFetchDiscoveryWorker(ctx context.Context, c *Container, cwd string) error {
	return runFetchDiscoveryWorkerWithWait(ctx, c, cwd, waitFetchDiscovery)
}

func fetchDiscoveryDelay(previous time.Duration, progressed bool) time.Duration {
	if progressed || previous == 0 {
		return time.Second
	}
	return min(previous*2, time.Minute)
}

func waitFetchDiscovery(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runFetchDiscoveryWorkerWithWait(ctx context.Context, c *Container, cwd string, wait func(context.Context, time.Duration) error) error {
	root, _, err := discoveryRoot(ctx, cwd)
	if err != nil || root == "" {
		return err
	}
	lock, err := openFetchWorkerLock(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	started := false
	for delay := time.Duration(0); ; {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil
			}
			return err
		}
		if !started {
			if err := providerfs.WriteRepoFileDurable(root, filepath.Join(fetchObservationDir, "launch"), []byte("0"), 0600); err != nil {
				return err
			}
			started = true
		}
		wake, _ := providerfs.ReadRepoFile(root, filepath.Join(fetchObservationDir, "wake"))
		pending, progressed, err := replayFetchObservationsWithProgress(ctx, c, cwd)
		mergePending, mergeProgress, mergeErr := replayAutomaticMergeDeliveries(ctx, c, cwd)
		pending, progressed, err = pending || mergePending, progressed || mergeProgress, errors.Join(err, mergeErr)
		if err != nil {
			hookWarn("fetch discovery remains unknown: %v", err)
		}
		if !pending {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			after, _ := providerfs.ReadRepoFile(root, filepath.Join(fetchObservationDir, "wake"))
			if string(wake) == string(after) {
				return err
			}
			continue
		}
		delay = fetchDiscoveryDelay(delay, progressed)
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}
