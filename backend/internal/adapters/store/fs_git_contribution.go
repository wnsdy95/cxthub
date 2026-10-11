package store

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitContributionStore = (*FSStore)(nil)

func validateGitContributionWrite(contribution domain.GitContribution) error {
	if contribution.ID != "" && contribution.ID != contribution.WithID().ID {
		return domain.ErrConflict
	}
	return contribution.Validate()
}

func validateGitContributionMemoryWrite(memory domain.GitContributionMemory) error {
	if memory.ID != "" && memory.ID != memory.WithID().ID {
		return domain.ErrConflict
	}
	return memory.Validate()
}

func (s *FSStore) readGitContribution(repo domain.ContentHash, id string) (domain.GitContribution, error) {
	contribution, err := readPRHistoryFile[domain.GitContribution](s.gitEvidencePath("git-contributions", repo, id))
	if err != nil {
		return domain.GitContribution{}, err
	}
	if contribution.RepoID != repo || contribution.ID != id {
		return domain.GitContribution{}, domain.ErrIntegrity
	}
	return contribution, nil
}

func (s *FSStore) PutGitContribution(ctx context.Context, contribution domain.GitContribution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGitContributionWrite(contribution); err != nil {
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
	return putPRContributionFile(ctx, s.gitEvidencePath("git-contributions", contribution.RepoID, contribution.ID), contribution)
}

func (s *FSStore) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
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
	contributions, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "git-contributions"), func(contribution domain.GitContribution) string {
		return s.gitEvidencePath("git-contributions", contribution.RepoID, contribution.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.GitContribution{}
	for _, contribution := range contributions {
		if contribution.RepoID == repo {
			result = append(result, contribution)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

func (s *FSStore) PutGitContributionMemory(ctx context.Context, memory domain.GitContributionMemory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGitContributionMemoryWrite(memory); err != nil {
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
	if _, err := s.readGitContribution(memory.RepoID, memory.ContributionID); err != nil {
		return err
	}
	return putPRContributionFile(ctx, s.gitEvidencePath("git-contribution-memories", memory.RepoID, memory.ID), memory)
}

func (s *FSStore) ListGitContributionMemories(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionMemory, error) {
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
	memories, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "git-contribution-memories"), func(memory domain.GitContributionMemory) string {
		return s.gitEvidencePath("git-contribution-memories", memory.RepoID, memory.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.GitContributionMemory{}
	for _, memory := range memories {
		if memory.RepoID != repo {
			continue
		}
		if _, err := s.readGitContribution(repo, memory.ContributionID); err != nil {
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
