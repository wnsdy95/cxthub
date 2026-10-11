package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.PRHistoryStore = (*FSStore)(nil)

func (s *FSStore) ListPRHistoryJobs(ctx context.Context, repo domain.ContentHash, origin, jobOrigin string, providerRepositoryID int64, now time.Time, limit int) ([]domain.PRPromotionJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePRHistoryJobsQuery(repo, origin, jobOrigin, providerRepositoryID, now, limit); err != nil {
		return nil, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	jobs, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "pr-jobs"), func(job domain.PRPromotionJob) string {
		return s.prJobPath(job.RepoID, job.ID)
	})
	if err != nil {
		return nil, err
	}
	type candidate struct {
		job         domain.PRPromotionJob
		nextAttempt time.Time
	}
	candidates := []candidate{}
	for _, job := range jobs {
		if job.RepoID != repo || job.GitOrigin != jobOrigin {
			continue
		}
		checkpoint, err := s.GetPRHistoryCheckpoint(ctx, repo, origin, providerRepositoryID, job.PR.Number)
		missing := errors.Is(err, domain.ErrNotFound)
		if err != nil && !missing {
			return nil, err
		}
		if missing || !checkpoint.NextAttempt.After(now) {
			nextAttempt := checkpoint.NextAttempt
			if missing {
				nextAttempt = job.CreatedAt
			}
			candidates = append(candidates, candidate{job: job, nextAttempt: nextAttempt})
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		if !candidates[left].nextAttempt.Equal(candidates[right].nextAttempt) {
			return candidates[left].nextAttempt.Before(candidates[right].nextAttempt)
		}
		return candidates[left].job.ID < candidates[right].job.ID
	})
	result := make([]domain.PRPromotionJob, 0, min(limit, len(candidates)))
	for _, candidate := range candidates[:min(limit, len(candidates))] {
		result = append(result, candidate.job)
	}
	return result, nil
}

func (s *FSStore) prHistoryCheckpointPath(repo domain.ContentHash, origin string, providerRepositoryID int64, number int) string {
	key, _ := json.Marshal([]string{string(repo), origin, strconv.FormatInt(providerRepositoryID, 10), strconv.Itoa(number)})
	return filepath.Join(s.dataDir, "pr-history-checkpoints", opaqueName(string(key))+".json")
}

func readPRHistoryFile[Record interface{ Validate() error }](path string) (Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		var record Record
		if errors.Is(err, os.ErrNotExist) {
			err = domain.ErrNotFound
		}
		return record, err
	}
	return decodePRHistory[Record](raw)
}

func (s *FSStore) GetPRHistoryCheckpoint(ctx context.Context, repo domain.ContentHash, origin string, providerRepositoryID int64, number int) (domain.PRHistoryCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if err := validatePRHistoryScope(repo, origin, number); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if providerRepositoryID <= 0 {
		return domain.PRHistoryCheckpoint{}, domain.ErrValidation
	}
	checkpoint, err := readPRHistoryFile[domain.PRHistoryCheckpoint](s.prHistoryCheckpointPath(repo, origin, providerRepositoryID, number))
	if err == nil {
		err = validatePRCheckpointScope(checkpoint, repo, origin, providerRepositoryID, number)
	}
	if err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	return checkpoint, nil
}

func (s *FSStore) SavePRHistoryCheckpoint(ctx context.Context, next domain.PRHistoryCheckpoint, expectedVersion int64) error {
	if err := validatePRCheckpointVersion(next, expectedVersion); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	release, err := s.pinWorkerRepoPolicy(ctx, next.RepoID)
	if err != nil {
		return err
	}
	defer release()
	old, err := s.GetPRHistoryCheckpoint(ctx, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number)
	if errors.Is(err, domain.ErrNotFound) {
		if expectedVersion != 0 {
			return domain.ErrConflict
		}
	} else if err != nil {
		return err
	} else if err = acceptPRCheckpoint(old, next, expectedVersion); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return writeAtomic(s.prHistoryCheckpointPath(next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number), raw)
}

func readPRHistoryDirectory[Record interface{ Validate() error }](ctx context.Context, directory string, pathFor func(Record) string) ([]Record, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := []Record{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		record, err := readPRHistoryFile[Record](path)
		if err != nil {
			return nil, err
		}
		if pathFor(record) != path {
			return nil, domain.ErrIntegrity
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *FSStore) ListPRRefObservations(ctx context.Context, repo domain.ContentHash, origin string, providerRepositoryID int64, number int, afterID string, limit int) ([]domain.GitRefObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePRObservationQuery(repo, origin, providerRepositoryID, number, afterID, limit); err != nil {
		return nil, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	observations, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "git-observations"), func(observation domain.GitRefObservation) string {
		return s.gitEvidencePath("git-observations", observation.RepoID, observation.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.GitRefObservation{}
	for _, observation := range observations {
		if observation.ID > afterID && matchesPRObservation(observation, repo, origin, providerRepositoryID, number) {
			result = append(result, observation)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result[:min(limit, len(result))], nil
}

func (s *FSStore) PutPRParticipation(ctx context.Context, participation domain.PRParticipation) error {
	if err := validatePRParticipationWrite(participation); err != nil {
		return err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	release, err := s.pinWorkerRepoPolicy(ctx, participation.RepoID)
	if err != nil {
		return err
	}
	defer release()
	path := s.gitEvidencePath("pr-participations", participation.RepoID, participation.ID)
	old, err := readPRHistoryFile[domain.PRParticipation](path)
	if err == nil {
		if old != participation {
			return domain.ErrIntegrity
		}
		return nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	raw, err := json.Marshal(participation)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func (s *FSStore) ListPRParticipations(ctx context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePRHistoryScope(repo, origin, number); err != nil {
		return nil, err
	}
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	participations, err := readPRHistoryDirectory(ctx, filepath.Join(s.dataDir, "pr-participations"), func(participation domain.PRParticipation) string {
		return s.gitEvidencePath("pr-participations", participation.RepoID, participation.ID)
	})
	if err != nil {
		return nil, err
	}
	result := []domain.PRParticipation{}
	for _, participation := range participations {
		if participation.RepoID == repo && participation.GitOrigin == origin && participation.Number == number {
			result = append(result, participation)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}
