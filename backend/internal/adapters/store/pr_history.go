package store

import (
	"encoding/json"
	"math"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func validatePRHistoryScope(repo domain.ContentHash, origin string, number int) error {
	if domain.ValidateContentHash(repo) != nil || origin == "" || number <= 0 {
		return domain.ErrValidation
	}
	return nil
}

func validatePRObservationQuery(repo domain.ContentHash, origin string, providerRepositoryID int64, number int, afterID string, limit int) error {
	if err := validatePRHistoryScope(repo, origin, number); err != nil {
		return err
	}
	if providerRepositoryID <= 0 || limit < 1 || limit > 101 || (afterID != "" && domain.ValidateGitChangeID(afterID) != nil) {
		return domain.ErrValidation
	}
	return nil
}

func validatePRHistoryJobsQuery(repo domain.ContentHash, origin, jobOrigin string, providerRepositoryID int64, now time.Time, limit int) error {
	if domain.ValidateContentHash(repo) != nil || origin == "" || jobOrigin == "" || providerRepositoryID <= 0 || now.IsZero() || limit < 1 || limit > 101 {
		return domain.ErrValidation
	}
	return nil
}

func validatePRCheckpointVersion(next domain.PRHistoryCheckpoint, expectedVersion int64) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || next.Version != expectedVersion+1 {
		return domain.ErrConflict
	}
	return nil
}

func acceptPRCheckpoint(old, next domain.PRHistoryCheckpoint, expectedVersion int64) error {
	if old.Version != expectedVersion {
		return domain.ErrConflict
	}
	if old.RepoID != next.RepoID || old.GitOrigin != next.GitOrigin || old.ProviderRepositoryID != next.ProviderRepositoryID || old.PR != next.PR || (old.PullRequestID != 0 && old.PullRequestID != next.PullRequestID) || (!old.MergedAt.IsZero() && !old.MergedAt.Equal(next.MergedAt)) {
		return domain.ErrIntegrity
	}
	return nil
}

func validatePRCheckpointScope(checkpoint domain.PRHistoryCheckpoint, repo domain.ContentHash, origin string, providerRepositoryID int64, number int) error {
	if checkpoint.RepoID != repo || checkpoint.GitOrigin != origin || checkpoint.ProviderRepositoryID != providerRepositoryID || checkpoint.PR.Number != number {
		return domain.ErrIntegrity
	}
	return nil
}

func matchesPRObservation(observation domain.GitRefObservation, repo domain.ContentHash, origin string, providerRepositoryID int64, number int) bool {
	return observation.RepoID == repo && observation.GitOrigin == origin && observation.PullRequest != nil && observation.PullRequest.RepositoryID == providerRepositoryID && observation.PullRequest.Number == number && (observation.Source == "github-pull-request" || observation.Source == "github-pr-reconciliation")
}

func validatePRParticipationWrite(participation domain.PRParticipation) error {
	if participation.ID != "" && participation.ID != participation.WithID().ID {
		return domain.ErrIntegrity
	}
	return participation.Validate()
}

func decodePRHistory[Record interface{ Validate() error }](raw []byte) (Record, error) {
	var record Record
	if json.Unmarshal(raw, &record) != nil || record.Validate() != nil {
		var empty Record
		return empty, domain.ErrIntegrity
	}
	return record, nil
}
