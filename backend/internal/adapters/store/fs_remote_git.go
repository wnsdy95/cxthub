package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RemoteGitStore = (*FSStore)(nil)

var fsRemoteGitLocks sync.Map

func (s *FSStore) remoteGitLock(repo domain.ContentHash) *sync.Mutex {
	lock, _ := fsRemoteGitLocks.LoadOrStore(s.dataDir+"\x00"+string(repo), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// The development store keeps everything read from a repository's provider
// in one file.
func (s *FSStore) remoteGitPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "remote-git.json")
}

type fsRemoteGit struct {
	State *domain.RemoteGitState `json:"state,omitempty"`
	// ClaimedUntil is the end of the claim a worker holds; the zero time when
	// nobody holds one.
	ClaimedUntil time.Time             `json:"claimed_until"`
	Commits      []domain.RemoteCommit `json:"commits"`
	Branches     []domain.RemoteBranch `json:"branches"`
	Pulls        []domain.RemotePull   `json:"pulls"`
	// Pushes are the times a branch was seen with another newest commit.
	Pushes []domain.RemoteBranchPush `json:"pushes,omitempty"`
}

// remoteGitCommit returns a commit as both stores hand it out: with a list of
// parents, empty for a root commit, and its times in UTC.
func remoteGitCommit(commit domain.RemoteCommit) domain.RemoteCommit {
	commit.Parents = append([]string{}, commit.Parents...)
	commit.AuthoredAt, commit.CommittedAt = commit.AuthoredAt.UTC(), commit.CommittedAt.UTC()
	return commit
}

func remoteGitPull(pull domain.RemotePull) domain.RemotePull {
	pull.Commits = append([]string{}, pull.Commits...)
	pull.UpdatedAt = pull.UpdatedAt.UTC()
	return pull
}

// remoteGitSHAs checks commit numbers a caller asks about and returns each
// once, in the order given.
func remoteGitSHAs(shas []string) ([]string, error) {
	seen := make(map[string]struct{}, len(shas))
	out := make([]string, 0, len(shas))
	for _, sha := range shas {
		if !domain.ValidRemoteCommitSHA(sha) {
			return nil, fmt.Errorf("%w: commit number", domain.ErrValidation)
		}
		if _, dup := seen[sha]; !dup {
			seen[sha] = struct{}{}
			out = append(out, sha)
		}
	}
	return out, nil
}

// remoteGitBranches checks a whole list of branches and returns it by name.
func remoteGitBranches(branches []domain.RemoteBranch) ([]domain.RemoteBranch, error) {
	out := append([]domain.RemoteBranch{}, branches...)
	sort.Slice(out, func(left, right int) bool { return out[left].Name < out[right].Name })
	for index, branch := range out {
		if err := branch.Validate(); err != nil {
			return nil, err
		}
		if index > 0 && out[index-1].Name == branch.Name {
			return nil, fmt.Errorf("%w: a branch name is listed twice", domain.ErrValidation)
		}
	}
	return out, nil
}

// remoteGitKeys checks keys a caller looks copies up by and returns each once.
func remoteGitKeys(keys []domain.RemoteCommitKey) ([]domain.RemoteCommitKey, error) {
	seen := make(map[domain.RemoteCommitKey]struct{}, len(keys))
	out := make([]domain.RemoteCommitKey, 0, len(keys))
	for _, key := range keys {
		if err := key.Validate(); err != nil {
			return nil, err
		}
		key.AuthoredAt = key.AuthoredAt.UTC()
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			out = append(out, key)
		}
	}
	return out, nil
}

func remoteGitLimit(limit int) error {
	if limit < 1 {
		return fmt.Errorf("%w: a list needs a positive limit", domain.ErrValidation)
	}
	return nil
}

func (s *FSStore) readRemoteGit(ctx context.Context, repo domain.ContentHash) (fsRemoteGit, error) {
	if err := validateHash(repo); err != nil {
		return fsRemoteGit{}, err
	}
	if err := ctx.Err(); err != nil {
		return fsRemoteGit{}, err
	}
	path := s.remoteGitPath(repo)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fsRemoteGit{}, nil
	}
	if err != nil {
		return fsRemoteGit{}, err
	}
	if !info.Mode().IsRegular() {
		return fsRemoteGit{}, fmt.Errorf("%w: remote git cache is not a regular file", domain.ErrIntegrity)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fsRemoteGit{}, err
	}
	var stored fsRemoteGit
	if err := json.Unmarshal(raw, &stored); err != nil {
		return fsRemoteGit{}, fmt.Errorf("%w: invalid remote git cache JSON: %v", domain.ErrIntegrity, err)
	}
	if err := stored.validate(repo); err != nil {
		return fsRemoteGit{}, fmt.Errorf("%w: invalid stored remote git cache: %v", domain.ErrIntegrity, err)
	}
	return stored, nil
}

// validate checks what both stores promise of what they hold.
func (stored fsRemoteGit) validate(repo domain.ContentHash) error {
	if stored.State != nil {
		if err := stored.State.Validate(); err != nil {
			return err
		}
		if stored.State.RepoID != repo {
			return fmt.Errorf("%w: reading state of another repository", domain.ErrIntegrity)
		}
	}
	commits := make(map[string]struct{}, len(stored.Commits))
	for _, commit := range stored.Commits {
		if err := commit.Validate(); err != nil {
			return err
		}
		if _, dup := commits[commit.SHA]; dup {
			return fmt.Errorf("%w: a commit is stored twice", domain.ErrConflict)
		}
		commits[commit.SHA] = struct{}{}
	}
	if _, err := remoteGitBranches(stored.Branches); err != nil {
		return err
	}
	pulls := make(map[int]struct{}, len(stored.Pulls))
	for _, pull := range stored.Pulls {
		if err := pull.Validate(); err != nil {
			return err
		}
		if _, dup := pulls[pull.Number]; dup {
			return fmt.Errorf("%w: a pull request is stored twice", domain.ErrConflict)
		}
		pulls[pull.Number] = struct{}{}
	}
	return nil
}

func (s *FSStore) writeRemoteGit(repo domain.ContentHash, stored fsRemoteGit) error {
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return writeAtomic(s.remoteGitPath(repo), raw)
}

// changeRemoteGit reads the cache, lets change edit it and writes it back when
// change says it changed.
func (s *FSStore) changeRemoteGit(ctx context.Context, repo domain.ContentHash, change func(*fsRemoteGit) (bool, error)) error {
	lock := s.remoteGitLock(repo)
	lock.Lock()
	defer lock.Unlock()
	stored, err := s.readRemoteGit(ctx, repo)
	if err != nil {
		return err
	}
	changed, err := change(&stored)
	if err != nil || !changed {
		return err
	}
	return s.writeRemoteGit(repo, stored)
}

func (s *FSStore) viewRemoteGit(ctx context.Context, repo domain.ContentHash) (fsRemoteGit, error) {
	lock := s.remoteGitLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return s.readRemoteGit(ctx, repo)
}

func (s *FSStore) GetRemoteGitState(ctx context.Context, repo domain.ContentHash) (domain.RemoteGitState, error) {
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return domain.RemoteGitState{}, err
	}
	if stored.State == nil {
		return domain.RemoteGitState{}, domain.ErrNotFound
	}
	return *stored.State, nil
}

func (s *FSStore) ClaimRemoteGitSync(ctx context.Context, repo domain.ContentHash, origin string, now time.Time, lease time.Duration) (domain.RemoteGitState, error) {
	if lease <= 0 {
		return domain.RemoteGitState{}, fmt.Errorf("%w: a claim needs a lease", domain.ErrValidation)
	}
	if err := domain.NewRemoteGitState(repo, origin, now).Validate(); err != nil {
		return domain.RemoteGitState{}, err
	}
	var claimed domain.RemoteGitState
	err := s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		state, startOver, ok := domain.ClaimRemoteGitPass(stored.State, stored.ClaimedUntil, repo, origin, now)
		if !ok {
			return false, domain.ErrNotFound
		}
		if startOver {
			*stored = fsRemoteGit{}
		}
		claimed = state
		stored.State, stored.ClaimedUntil = &state, now.Add(lease).UTC()
		return true, nil
	})
	if err != nil {
		return domain.RemoteGitState{}, err
	}
	return claimed, nil
}

func (s *FSStore) FinishRemoteGitSync(ctx context.Context, state domain.RemoteGitState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return s.changeRemoteGit(ctx, state.RepoID, func(stored *fsRemoteGit) (bool, error) {
		if stored.State == nil {
			return false, domain.ErrNotFound
		}
		stored.State, stored.ClaimedUntil = &state, time.Time{}
		return true, nil
	})
}

func (s *FSStore) RequestRemoteGitSync(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	return s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		if stored.State == nil {
			return false, nil
		}
		next := stored.State.Request(now)
		stored.State = &next
		return true, nil
	})
}

func (s *FSStore) PutRemoteCommits(ctx context.Context, repo domain.ContentHash, commits []domain.RemoteCommit) (int, error) {
	for _, commit := range commits {
		if err := commit.Validate(); err != nil {
			return 0, err
		}
	}
	added := 0
	err := s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		known := make(map[string]struct{}, len(stored.Commits))
		for _, commit := range stored.Commits {
			known[commit.SHA] = struct{}{}
		}
		for _, commit := range commits {
			if _, present := known[commit.SHA]; present {
				continue
			}
			known[commit.SHA] = struct{}{}
			stored.Commits = append(stored.Commits, remoteGitCommit(commit))
			added++
		}
		return added > 0, nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

func (stored fsRemoteGit) commits() map[string]domain.RemoteCommit {
	bySHA := make(map[string]domain.RemoteCommit, len(stored.Commits))
	for _, commit := range stored.Commits {
		bySHA[commit.SHA] = commit
	}
	return bySHA
}

func (s *FSStore) MissingRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]string, error) {
	asked, err := remoteGitSHAs(shas)
	if err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	known, missing := stored.commits(), []string{}
	for _, sha := range asked {
		if _, present := known[sha]; !present {
			missing = append(missing, sha)
		}
	}
	return missing, nil
}

func (s *FSStore) GetRemoteCommits(ctx context.Context, repo domain.ContentHash, shas []string) ([]domain.RemoteCommit, error) {
	asked, err := remoteGitSHAs(shas)
	if err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	known, found := stored.commits(), []domain.RemoteCommit{}
	for _, sha := range asked {
		if commit, present := known[sha]; present {
			found = append(found, remoteGitCommit(commit))
		}
	}
	return found, nil
}

func (s *FSStore) ListRemoteCommits(ctx context.Context, repo domain.ContentHash, before domain.RemoteCommitCursor, limit int) ([]domain.RemoteCommit, error) {
	if err := remoteGitLimit(limit); err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	listed := append([]domain.RemoteCommit{}, stored.Commits...)
	sort.Slice(listed, func(left, right int) bool {
		if !listed[left].CommittedAt.Equal(listed[right].CommittedAt) {
			return listed[left].CommittedAt.After(listed[right].CommittedAt)
		}
		return listed[left].SHA > listed[right].SHA
	})
	page := []domain.RemoteCommit{}
	for _, commit := range listed {
		if len(page) == limit {
			break
		}
		if !before.IsZero() && (commit.CommittedAt.After(before.CommittedAt) || (commit.CommittedAt.Equal(before.CommittedAt) && commit.SHA >= before.SHA)) {
			continue
		}
		page = append(page, remoteGitCommit(commit))
	}
	return page, nil
}

func (s *FSStore) SearchRemoteCommits(ctx context.Context, repo domain.ContentHash, query string, limit int) ([]domain.RemoteCommit, error) {
	if err := remoteGitLimit(limit); err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	found, query := []domain.RemoteCommit{}, strings.ToLower(query)
	if query == "" {
		return found, nil
	}
	for _, commit := range stored.Commits {
		if strings.Contains(strings.ToLower(commit.Subject), query) || strings.HasPrefix(commit.SHA, query) {
			found = append(found, remoteGitCommit(commit))
		}
	}
	sort.Slice(found, func(left, right int) bool {
		if !found[left].CommittedAt.Equal(found[right].CommittedAt) {
			return found[left].CommittedAt.After(found[right].CommittedAt)
		}
		return found[left].SHA > found[right].SHA
	})
	if len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

func (s *FSStore) RemoteCommitFrontier(ctx context.Context, repo domain.ContentHash, limit int) ([]string, error) {
	if err := remoteGitLimit(limit); err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	skipped := map[string]struct{}{}
	if stored.State != nil {
		for _, sha := range stored.State.Unreadable {
			skipped[sha] = struct{}{}
		}
	}
	known, frontier := stored.commits(), []string{}
	for _, commit := range stored.Commits {
		for _, parent := range commit.Parents {
			if _, present := known[parent]; present {
				continue
			}
			if _, skip := skipped[parent]; skip {
				continue
			}
			skipped[parent] = struct{}{}
			frontier = append(frontier, parent)
		}
	}
	sort.Strings(frontier)
	if len(frontier) > limit {
		frontier = frontier[:limit]
	}
	return frontier, nil
}

func (s *FSStore) FindRemoteCommitCopies(ctx context.Context, repo domain.ContentHash, keys []domain.RemoteCommitKey) ([]domain.RemoteCommit, error) {
	wanted, err := remoteGitKeys(keys)
	if err != nil {
		return nil, err
	}
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	asked := make(map[domain.RemoteCommitKey]struct{}, len(wanted))
	for _, key := range wanted {
		asked[key] = struct{}{}
	}
	copies := []domain.RemoteCommit{}
	for _, commit := range stored.Commits {
		if _, match := asked[commit.Key()]; match {
			copies = append(copies, remoteGitCommit(commit))
		}
	}
	sort.Slice(copies, func(left, right int) bool { return copies[left].SHA < copies[right].SHA })
	return copies, nil
}

func (s *FSStore) ReplaceRemoteBranches(ctx context.Context, repo domain.ContentHash, branches []domain.RemoteBranch) error {
	listed, err := remoteGitBranches(branches)
	if err != nil {
		return err
	}
	return s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		stored.Branches = listed
		return true, nil
	})
}

func (s *FSStore) ListRemoteBranches(ctx context.Context, repo domain.ContentHash) ([]domain.RemoteBranch, error) {
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	return remoteGitBranches(stored.Branches)
}

func (s *FSStore) PutRemotePull(ctx context.Context, repo domain.ContentHash, pull domain.RemotePull) error {
	if err := pull.Validate(); err != nil {
		return err
	}
	return s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		kept := make([]domain.RemotePull, 0, len(stored.Pulls)+1)
		for _, existing := range stored.Pulls {
			if existing.Number != pull.Number {
				kept = append(kept, existing)
			}
		}
		stored.Pulls = append(kept, remoteGitPull(pull))
		return true, nil
	})
}

func (s *FSStore) ListRemotePulls(ctx context.Context, repo domain.ContentHash) ([]domain.RemotePull, error) {
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	pulls := make([]domain.RemotePull, 0, len(stored.Pulls))
	for _, pull := range stored.Pulls {
		pulls = append(pulls, remoteGitPull(pull))
	}
	sort.Slice(pulls, func(left, right int) bool { return pulls[left].Number > pulls[right].Number })
	return pulls, nil
}

var _ outbound.RemotePushStore = (*FSStore)(nil)

func (s *FSStore) RecordRemotePushes(ctx context.Context, repo domain.ContentHash, pushes []domain.RemoteBranchPush) error {
	for _, push := range pushes {
		if err := push.Validate(); err != nil {
			return err
		}
	}
	if len(pushes) == 0 {
		return nil
	}
	return s.changeRemoteGit(ctx, repo, func(stored *fsRemoteGit) (bool, error) {
		type sighting struct {
			branch string
			at     time.Time
		}
		seen := make(map[sighting]struct{}, len(stored.Pushes))
		for _, push := range stored.Pushes {
			seen[sighting{push.Branch, push.SeenAt.UTC()}] = struct{}{}
		}
		changed := false
		for _, push := range pushes {
			push.SeenAt = push.SeenAt.UTC()
			key := sighting{push.Branch, push.SeenAt}
			if _, had := seen[key]; had {
				continue
			}
			seen[key] = struct{}{}
			stored.Pushes = append(stored.Pushes, push)
			changed = true
		}
		return changed, nil
	})
}

func (s *FSStore) ListRemotePushes(ctx context.Context, repo domain.ContentHash, branches []string) ([]domain.RemoteBranchPush, error) {
	stored, err := s.viewRemoteGit(ctx, repo)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		wanted[branch] = struct{}{}
	}
	pushes := []domain.RemoteBranchPush{}
	for _, push := range stored.Pushes {
		if _, ok := wanted[push.Branch]; ok {
			push.SeenAt = push.SeenAt.UTC()
			pushes = append(pushes, push)
		}
	}
	sortRemotePushes(pushes)
	return pushes, nil
}

func sortRemotePushes(pushes []domain.RemoteBranchPush) {
	sort.Slice(pushes, func(left, right int) bool {
		a, b := pushes[left], pushes[right]
		if !a.SeenAt.Equal(b.SeenAt) {
			return a.SeenAt.Before(b.SeenAt)
		}
		if a.Branch != b.Branch {
			return a.Branch < b.Branch
		}
		return a.SHA < b.SHA
	})
}
