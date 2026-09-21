package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryBindings = (*FSStore)(nil)

var repositoryBindingLocks sync.Map

func (s *FSStore) repositoryBindingLock(id string) *sync.Mutex {
	lock, _ := repositoryBindingLocks.LoadOrStore(s.dataDir+"\x00"+id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) GetBoundRepo(ctx context.Context, id string) (domain.Repo, error) {
	if err := domain.ValidateRepositoryID(id); err != nil {
		return domain.Repo{}, err
	}
	list, err := s.ListRepos(ctx, "default")
	if err != nil {
		return domain.Repo{}, err
	}
	var found domain.Repo
	for _, r := range list {
		if r.RepositoryID == id {
			if found.ID != "" {
				return domain.Repo{}, domain.ErrIntegrity
			}
			found = r
		}
	}
	if found.ID == "" {
		return found, domain.ErrNotFound
	}
	return found, nil
}
func (s *FSStore) InviteTargets(ctx context.Context, token string) ([]string, error) {
	inv, err := s.GetInvite(ctx, token)
	if err != nil {
		return nil, err
	}
	var targets []string
	if err = readJSON(filepath.Join(s.dataDir, "repository-invite-targets", token+".json"), &targets); errors.Is(err, domain.ErrNotFound) {
		return []string{inv.RepositoryID}, nil
	} else if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, domain.ErrIntegrity
	}
	for _, id := range targets {
		if err = domain.ValidateRepositoryID(id); err != nil {
			return nil, err
		}
	}
	return targets, nil
}
