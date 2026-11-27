package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.BranchLedgerStore = (*FSStore)(nil)

var fsBranchLedgerLocks sync.Map

func (s *FSStore) branchLedgerLock(repo domain.ContentHash) *sync.Mutex {
	lock, _ := fsBranchLedgerLocks.LoadOrStore(s.dataDir+"\x00"+string(repo), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// The development store keeps a repository's whole ledger in one file.
func (s *FSStore) branchLedgerPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "branch-ledger.json")
}

// validateBranchLedger checks what both stores promise: every lifetime is
// well formed, every ID belongs to one lifetime, and one lifetime is alive
// under a name.
func validateBranchLedger(repo domain.ContentHash, lifetimes []domain.BranchLifetime) error {
	ids, alive := map[string]struct{}{}, map[string]struct{}{}
	for _, lifetime := range lifetimes {
		if err := lifetime.Validate(); err != nil {
			return err
		}
		if lifetime.RepoID != repo {
			return fmt.Errorf("%w: branch lifetime of another repository", domain.ErrIntegrity)
		}
		for _, id := range lifetime.IDs() {
			if _, dup := ids[id]; dup {
				return fmt.Errorf("%w: a branch ID belongs to two lifetimes", domain.ErrConflict)
			}
			ids[id] = struct{}{}
		}
		if lifetime.Alive() {
			if _, dup := alive[lifetime.Name]; dup {
				return fmt.Errorf("%w: two lifetimes are alive under one branch name", domain.ErrConflict)
			}
			alive[lifetime.Name] = struct{}{}
		}
	}
	return nil
}

func sortBranchLifetimes(lifetimes []domain.BranchLifetime) {
	sort.Slice(lifetimes, func(left, right int) bool {
		if !lifetimes[left].OpenedAt.Equal(lifetimes[right].OpenedAt) {
			return lifetimes[left].OpenedAt.Before(lifetimes[right].OpenedAt)
		}
		return lifetimes[left].LedgerID < lifetimes[right].LedgerID
	})
}

func (s *FSStore) readBranchLedger(ctx context.Context, repo domain.ContentHash) ([]domain.BranchLifetime, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := s.branchLedgerPath(repo)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.BranchLifetime{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: branch ledger is not a regular file", domain.ErrIntegrity)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lifetimes := []domain.BranchLifetime{}
	if err := json.Unmarshal(raw, &lifetimes); err != nil {
		return nil, fmt.Errorf("%w: invalid branch ledger JSON: %v", domain.ErrIntegrity, err)
	}
	if err := validateBranchLedger(repo, lifetimes); err != nil {
		return nil, fmt.Errorf("%w: invalid stored branch ledger: %v", domain.ErrIntegrity, err)
	}
	sortBranchLifetimes(lifetimes)
	return lifetimes, nil
}

func (s *FSStore) AliveBranchLifetime(ctx context.Context, repo domain.ContentHash, name string) (domain.BranchLifetime, error) {
	lock := s.branchLedgerLock(repo)
	lock.Lock()
	defer lock.Unlock()
	lifetimes, err := s.readBranchLedger(ctx, repo)
	if err != nil {
		return domain.BranchLifetime{}, err
	}
	for _, lifetime := range lifetimes {
		if lifetime.Name == name && lifetime.Alive() {
			return lifetime, nil
		}
	}
	return domain.BranchLifetime{}, domain.ErrNotFound
}

func (s *FSStore) BranchLifetimeHolding(ctx context.Context, repo domain.ContentHash, id string) (domain.BranchLifetime, error) {
	lock := s.branchLedgerLock(repo)
	lock.Lock()
	defer lock.Unlock()
	lifetimes, err := s.readBranchLedger(ctx, repo)
	if err != nil {
		return domain.BranchLifetime{}, err
	}
	for _, lifetime := range lifetimes {
		if lifetime.Holds(id) {
			return lifetime, nil
		}
	}
	return domain.BranchLifetime{}, domain.ErrNotFound
}

func (s *FSStore) ListBranchLifetimes(ctx context.Context, repo domain.ContentHash, name string) ([]domain.BranchLifetime, error) {
	lock := s.branchLedgerLock(repo)
	lock.Lock()
	defer lock.Unlock()
	lifetimes, err := s.readBranchLedger(ctx, repo)
	if err != nil || name == "" {
		return lifetimes, err
	}
	out := []domain.BranchLifetime{}
	for _, lifetime := range lifetimes {
		if lifetime.Name == name {
			out = append(out, lifetime)
		}
	}
	return out, nil
}

func (s *FSStore) PutBranchLifetime(ctx context.Context, lifetime domain.BranchLifetime) error {
	if err := lifetime.Validate(); err != nil {
		return err
	}
	lock := s.branchLedgerLock(lifetime.RepoID)
	lock.Lock()
	defer lock.Unlock()
	lifetimes, err := s.readBranchLedger(ctx, lifetime.RepoID)
	if err != nil {
		return err
	}
	next := make([]domain.BranchLifetime, 0, len(lifetimes)+1)
	for _, existing := range lifetimes {
		if existing.LedgerID != lifetime.LedgerID {
			next = append(next, existing)
		}
	}
	next = append(next, lifetime)
	if err := validateBranchLedger(lifetime.RepoID, next); err != nil {
		return err
	}
	sortBranchLifetimes(next)
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return writeAtomic(s.branchLedgerPath(lifetime.RepoID), raw)
}
