package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

const capturePushDir = ".cxt/capture/push-requests"

// CapturePushDestination is an observed destination, not a remote selector.
// APIOnly preserves the documented CXT_REMOTE API-base fallback semantics.
// Credentials stay in the prepared transport and never enter the intent.
type CapturePushDestination struct {
	URL     string
	APIOnly bool
}

// A pre-push intent is separate from capture completion: recording a commit
// alone never authorizes advancing a remote branch. Destinations are hashed so
// credential-bearing remote URLs cannot leak into the receipt.
type capturePushIntent struct {
	Version                            int
	RepoID, AttemptID, WorktreeID      string
	Branch                             domain.PublicationBranch
	Update                             gitPushUpdate
	GitDestination, ContextDestination domain.ContentHash
	ContextAPIOnly                     bool `json:",omitempty"`
}

type capturePushRequest struct {
	Intent capturePushIntent
	Tries  int
	Next   time.Time
	Done   bool
	Error  string
}

func (p capturePushIntent) id() string {
	raw, _ := json.Marshal(p)
	return strings.TrimPrefix(string(domain.HashContent(raw)), "sha256:")
}

func capturePushDestinations(ctx context.Context, cwd, root string) (domain.ContentHash, CapturePushDestination, error) {
	var destination CapturePushDestination
	git, err := exec.CommandContext(ctx, "git", "-C", cwd, "remote", "get-url", "--push", "--all", "origin").Output()
	if err != nil {
		return "", destination, fmt.Errorf("Git origin unavailable")
	}
	remotes, err := remotecfg.Load(root)
	if err != nil {
		return "", destination, err
	}
	destination.URL = remotes["origin"]
	if destination.URL == "" {
		destination.URL, destination.APIOnly = os.Getenv("CXT_REMOTE"), true
	}
	if destination.URL == "" {
		return "", destination, fmt.Errorf("context origin unavailable")
	}
	return domain.HashContent(git), destination, nil
}

func queueCapturePush(ctx context.Context, c *Container, cwd string, updates []gitPushUpdate, scope domain.PublicationScope) error {
	root := cxtRepoRoot(ctx, cwd)
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return err
	}
	attempts, err := capturejournal.New(root, cwd).ListCaptureAttempts(ctx, repo.ID)
	if err != nil {
		return err
	}
	for _, update := range updates {
		binding, err := c.History.ResolveLocalBranch(ctx, repo.ID, strings.TrimPrefix(update.LocalRef, "refs/heads/"))
		if err != nil {
			return err
		}
		branch := domain.PublicationBranch{Branch: binding.Branch, BranchID: binding.BranchID}
		allowed := false
		for _, b := range scope.Branches {
			if b == branch {
				allowed = true
			}
		}
		if !allowed {
			return domain.ErrSyncConflict
		}
		var match *domain.CaptureAttempt
		for i := range attempts {
			p := &attempts[i]
			if p.Version != 2 || !p.InputsReady || p.Proof.BranchID != branch.BranchID || p.Proof.Branch != branch.Branch || p.Proof.GitAfter != strings.ToLower(update.LocalOID) {
				continue
			}
			if match != nil {
				return fmt.Errorf("multiple captures at pushed revision require explicit selection")
			}
			match = p
		}
		if match == nil {
			continue
		}
		git, remote, err := capturePushDestinations(ctx, cwd, root)
		if err != nil {
			return err
		}
		intent := capturePushIntent{Version: 1, RepoID: repo.ID, AttemptID: match.Proof.ID, WorktreeID: match.Proof.WorktreeID, Branch: branch, Update: update, GitDestination: git, ContextDestination: domain.HashContent([]byte(remote.URL)), ContextAPIOnly: remote.APIOnly}
		_, err = providerfs.WithCxtLock(ctx, root, "capture-push", intent.id(), syscall.LOCK_EX, true, func() error {
			rel := filepath.Join(capturePushDir, intent.id()+".json")
			if raw, err := providerfs.ReadRepoFile(root, rel); err == nil {
				var old capturePushRequest
				if json.Unmarshal(raw, &old) != nil || old.Intent != intent {
					return domain.ErrHashMismatch
				}
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			raw, _ := json.Marshal(capturePushRequest{Intent: intent})
			return providerfs.WriteRepoFileDurable(root, rel, raw, 0600)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Atomic writes can be visible during enqueue or remain after a killed writer.
// Inspect only their names and lstat metadata: never open/follow or delete them.
func capturePushAtomicTemp(dir string, entry os.DirEntry) (bool, error) {
	suffix, ok := strings.CutPrefix(entry.Name(), ".cxt-write-")
	if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
		return false, nil
	}
	if _, err := strconv.ParseUint(suffix, 10, 32); err != nil {
		return false, nil
	}
	if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
		return false, domain.ErrHashMismatch
	}
	info, err := os.Lstat(filepath.Join(dir, entry.Name()))
	if os.IsNotExist(err) {
		// A concurrent durable rename removed an observed staging file.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, domain.ErrHashMismatch
	}
	return true, nil
}

// Called under the capture worker's permanent-inode lock. Claims precede
// network work; crashes and concurrent wakes cannot reset the bounded retry.
func drainCapturePush(ctx context.Context, c *Container, cwd, root, repoID string) (time.Time, error) {
	var next time.Time
	dir := filepath.Join(root, capturePushDir)
	if err := providerfs.ValidateCxtDir(dir); err != nil {
		return next, err
	}
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return next, nil
	}
	if err != nil {
		return next, err
	}
	for _, file := range files {
		if skip, err := capturePushAtomicTemp(dir, file); err != nil {
			return next, err
		} else if skip {
			continue
		}
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 || filepath.Ext(file.Name()) != ".json" {
			return next, domain.ErrHashMismatch
		}
		rel := filepath.Join(capturePushDir, file.Name())
		f, err := providerfs.OpenRepoFile(root, rel)
		if err != nil {
			return next, err
		}
		raw, err := io.ReadAll(io.LimitReader(f, 65537))
		_ = f.Close()
		if err != nil {
			return next, err
		}
		var r capturePushRequest
		if len(raw) > 65536 || json.Unmarshal(raw, &r) != nil || r.Intent.Version != 1 || r.Intent.RepoID != repoID || r.Intent.id()+".json" != file.Name() || r.Tries < 0 || r.Tries > 8 {
			return next, domain.ErrHashMismatch
		}
		if r.Done || r.Tries >= 8 {
			continue
		}
		// Waiting for a capture consumes no transport attempt: a large input may
		// legitimately take longer than the network retry budget.
		p, err := capturePushAttempt(root, r.Intent)
		if err != nil {
			return next, err
		}
		if !p.Complete || !p.MemoryFinalized {
			continue
		}
		if time.Now().Before(r.Next) {
			if next.IsZero() || r.Next.Before(next) {
				next = r.Next
			}
			continue
		}
		r.Tries++
		r.Next = time.Now().Add(time.Duration(1<<r.Tries) * time.Second)
		raw, _ = json.Marshal(r)
		if err := providerfs.WriteRepoFileDurable(root, rel, raw, 0600); err != nil {
			return next, err
		}
		job, cancel := context.WithTimeout(ctx, time.Minute)
		err = deliverCapturePush(job, c, cwd, root, r.Intent, p)
		cancel()
		r.Error = ""
		if err == nil {
			r.Done = true
		} else {
			r.Error = err.Error()
		}
		raw, _ = json.Marshal(r)
		if err := providerfs.WriteRepoFileDurable(root, rel, raw, 0600); err != nil {
			return next, err
		}
		if !r.Done && r.Tries < 8 && (next.IsZero() || r.Next.Before(next)) {
			next = r.Next
		}
	}
	return next, nil
}

func capturePushAttempt(root string, intent capturePushIntent) (domain.CaptureAttempt, error) {
	var p domain.CaptureAttempt
	// IDs are not paths. Validate the envelope before joining private paths.
	for _, id := range []string{intent.AttemptID, intent.WorktreeID} {
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			return p, domain.ErrHashMismatch
		}
	}
	raw, err := providerfs.ReadRepoFile(root, filepath.Join(".cxt", "worktrees", intent.WorktreeID, "capture-passes", intent.AttemptID+".json"))
	if err != nil {
		return p, err
	}
	if json.Unmarshal(raw, &p) != nil || p.Validate() != nil || p.Version != 2 || !p.InputsReady || p.Proof.ID != intent.AttemptID || p.Proof.WorktreeID != intent.WorktreeID || p.Proof.RepoID != intent.RepoID || p.Proof.Branch != intent.Branch.Branch || p.Proof.BranchID != intent.Branch.BranchID || p.Proof.GitAfter != strings.ToLower(intent.Update.LocalOID) {
		return p, domain.ErrHashMismatch
	}
	return p, nil
}

func deliverCapturePush(ctx context.Context, c *Container, cwd, root string, intent capturePushIntent, p domain.CaptureAttempt) error {
	if c.PrepareCapturePush == nil {
		return fmt.Errorf("bound context publication service unavailable")
	}
	updates := []gitPushUpdate{intent.Update}
	var sync inbound.SyncRepo
	for attempt := 0; attempt < 2; attempt++ {
		git, remote, err := capturePushDestinations(ctx, cwd, root)
		if err != nil {
			return err
		}
		if git != intent.GitDestination || domain.HashContent([]byte(remote.URL)) != intent.ContextDestination || remote.APIOnly != intent.ContextAPIOnly {
			return fmt.Errorf("push destination changed; deferred publication retained")
		}
		if err := checkGitPushObjects(ctx, cwd, updates); err != nil {
			return err
		}
		scope, err := resolveGitPushScope(ctx, c, cwd, updates)
		if err != nil {
			return err
		}
		if len(scope.Branches) != 1 || scope.Branches[0] != intent.Branch {
			return domain.ErrSyncConflict
		}
		if err := checkGitPushAliasProof(ctx, c, cwd, updates); err != nil {
			return err
		}
		history, err := publicationHistory(ctx, c, intent.RepoID)
		if err != nil {
			return err
		}
		want := p.Publication()
		if got, ok := history[want.ID]; !ok || domain.ValidateHistoryEvent(got) != nil || domain.CapturePublicationID(got) != want.ID {
			return fmt.Errorf("completed capture publication has not been recorded")
		}
		if domain.ValidateContentHash(p.Proof.Target) != nil {
			return domain.ErrHashMismatch
		}
		if sync == nil {
			// Pass the exact observed URL, never ask the factory to select origin.
			// The returned transport owns fixed endpoint/credential/repo bindings
			// for every request, including the one bounded append retry.
			sync, err = c.PrepareCapturePush(ctx, cwd, remote, intent.RepoID)
			if err != nil {
				return err
			}
			if sync == nil {
				return fmt.Errorf("bound context publication service unavailable")
			}
		}
		scope.ExpectedTargets = map[string]domain.ContentHash{intent.Branch.BranchID: p.Proof.Target}
		_, err = sync.Push(ctx, inbound.SyncInput{Cwd: cwd, RepoID: intent.RepoID, ForegroundOnly: true, Append: attempt == 1, Publication: &scope})
		if !errors.Is(err, domain.ErrSyncConflict) {
			return err
		}
	}
	return domain.ErrSyncConflict
}
