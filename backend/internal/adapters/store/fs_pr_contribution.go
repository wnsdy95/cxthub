package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.PRContributionStore = (*FSStore)(nil)

func validatePRRefObservationLookup(repo domain.ContentHash, origin, id string) error {
	if domain.ValidateContentHash(repo) != nil || origin == "" || domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.ErrValidation
	}
	return nil
}

func validatePRRefObservationIdentity(observation domain.GitRefObservation, repo domain.ContentHash, origin, id string) error {
	if observation.RepoID != repo || observation.GitOrigin != origin || observation.ID != id {
		return domain.ErrIntegrity
	}
	return nil
}

func (s *FSStore) GetPRRefObservation(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitRefObservation, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitRefObservation{}, err
	}
	if err := validatePRRefObservationLookup(repo, origin, id); err != nil {
		return domain.GitRefObservation{}, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.GitRefObservation{}, err
	}
	observation, err := readPRHistoryFile[domain.GitRefObservation](s.gitEvidencePath("git-observations", repo, id))
	if err != nil {
		return domain.GitRefObservation{}, err
	}
	if err := validatePRRefObservationIdentity(observation, repo, origin, id); err != nil {
		return domain.GitRefObservation{}, err
	}
	return observation, nil
}

func validatePRContributionWrite(contribution domain.PRContribution) error {
	if contribution.ID != "" && contribution.ID != contribution.WithID().ID {
		return domain.ErrConflict
	}
	return contribution.Validate()
}

func validatePRContributionMemoryWrite(memory domain.PRContributionMemory) error {
	if memory.ID != "" && memory.ID != memory.WithID().ID {
		return domain.ErrConflict
	}
	return memory.Validate()
}

func putPRContributionFile[Record interface {
	comparable
	Validate() error
}](ctx context.Context, path string, next Record) error {
	old, err := readPRHistoryFile[Record](path)
	if err == nil {
		if old != next {
			return domain.ErrConflict
		}
		return ctx.Err()
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func (s *FSStore) readPRContribution(repo domain.ContentHash, id string) (domain.PRContribution, error) {
	contribution, err := readPRHistoryFile[domain.PRContribution](s.gitEvidencePath("pr-contributions", repo, id))
	if err != nil {
		return domain.PRContribution{}, err
	}
	if contribution.RepoID != repo || contribution.ID != id {
		return domain.PRContribution{}, domain.ErrIntegrity
	}
	return contribution, nil
}

func (s *FSStore) PutPRContribution(ctx context.Context, contribution domain.PRContribution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePRContributionWrite(contribution); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	release, err := s.pinWorkerRepoPolicy(ctx, contribution.RepoID)
	if err != nil {
		return err
	}
	defer release()
	return putPRContributionFile(ctx, s.gitEvidencePath("pr-contributions", contribution.RepoID, contribution.ID), contribution)
}

func (s *FSStore) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contributions, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "pr-contributions"), func(contribution domain.PRContribution) string {
		return s.gitEvidencePath("pr-contributions", contribution.RepoID, contribution.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.PRContribution{}
	for _, contribution := range contributions {
		if contribution.RepoID == repo {
			result = append(result, contribution)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

func (s *FSStore) PutPRContributionMemory(ctx context.Context, memory domain.PRContributionMemory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePRContributionMemoryWrite(memory); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	release, err := s.pinWorkerRepoPolicy(ctx, memory.RepoID)
	if err != nil {
		return err
	}
	defer release()
	if _, err := s.readPRContribution(memory.RepoID, memory.ContributionID); err != nil {
		return err
	}
	return putPRContributionFile(ctx, s.gitEvidencePath("pr-contribution-memories", memory.RepoID, memory.ID), memory)
}

func (s *FSStore) ListPRContributionMemories(ctx context.Context, repo domain.ContentHash) ([]domain.PRContributionMemory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return nil, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memories, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "pr-contribution-memories"), func(memory domain.PRContributionMemory) string {
		return s.gitEvidencePath("pr-contribution-memories", memory.RepoID, memory.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.PRContributionMemory{}
	for _, memory := range memories {
		if memory.RepoID != repo {
			continue
		}
		if _, err := s.readPRContribution(repo, memory.ContributionID); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, domain.ErrIntegrity
			}
			return nil, err
		}
		result = append(result, memory)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}
