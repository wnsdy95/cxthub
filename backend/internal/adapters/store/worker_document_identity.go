package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// The queue lock is already held. This is the same metadata lock used by opt-in
// and PutRepo; retain it until the queue transition finishes.
func (s *FSStore) pinWorkerRepoPolicy(ctx context.Context, repo domain.ContentHash) (func(), error) {
	lock := s.refLock(repo, domain.RefBranch, "")
	lock.Lock()
	r, err := s.GetRepo(ctx, repo)
	// Older development fixtures/layouts have unregistered legacy queues.
	if errors.Is(err, domain.ErrNotFound) {
		err = nil
	}
	if err == nil {
		err = outbound.CheckDocumentIdentityCompatibility(ctx, r.RequiredDocIdentity)
	}
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	return lock.Unlock, nil
}

// Pin binding creation as well as an existing bound repo's current requirement.
func (s *FSStore) pinWorkerRepositoryPolicy(ctx context.Context, repository string) (func(), error) {
	binding := s.repositoryBindingLock(repository)
	binding.Lock()
	repos, err := s.ListRepos(ctx, "default")
	var found domain.ContentHash
	for _, r := range repos {
		if r.RepositoryID == repository {
			if found != "" {
				err = domain.ErrIntegrity
				break
			}
			found = r.ID
		}
	}
	if err != nil {
		binding.Unlock()
		return nil, err
	}
	if found == "" {
		if err = outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityLegacy); err != nil {
			binding.Unlock()
			return nil, err
		}
		return binding.Unlock, nil
	}
	release, err := s.pinWorkerRepoPolicy(ctx, found)
	if err != nil {
		binding.Unlock()
		return nil, err
	}
	return func() { release(); binding.Unlock() }, nil
}

// Selection is only an optimization. The actual transition rechecks/pins the
// current requirement; unknown identities are never selected as legacy.
func workerDocumentRequirements(ctx context.Context) ([]string, error) {
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityLegacy); err != nil {
		return nil, err
	}
	allowed := []string{string(domain.DocumentIdentityLegacy)}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1); err == nil {
		allowed = append(allowed, string(domain.DocumentIdentityRootV1))
	} else if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		return nil, err
	}
	return allowed, nil
}
