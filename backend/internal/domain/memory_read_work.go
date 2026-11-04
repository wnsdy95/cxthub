package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

const (
	MemoryReadWorkVersion     = 1
	MaxMemoryReadTargets      = 128
	MaxPendingMemoryReadWork  = 32
	MaxMemoryReadWorkAttempts = 6
	MaxMemoryReadTargetBytes  = 16 << 10
	MaxMemoryReadWorkerRepos  = 128
)

var ErrMemoryReadNotReady = errors.New("memory read model is being prepared")
var ErrMemoryReadNeedsAttention = errors.New("memory read preparation requires attention")

type MemoryReadTarget struct {
	Version   int                      `json:"version"`
	Rules     int                      `json:"rules"`
	RepoID    ContentHash              `json:"repo_id"`
	Selection EffectiveMemorySelection `json:"selection"`
	Content   string                   `json:"content"`
}

func (target MemoryReadTarget) Validate() error {
	if target.Version != MemoryReadWorkVersion || target.Rules != MemoryReadRules || ValidateContentHash(target.RepoID) != nil ||
		target.Selection.Validate() != nil || !utf8.ValidString(target.Selection.Branch) ||
		(target.Content != "" && target.Content != "claims" && target.Content != "prompt") {
		return ErrValidation
	}
	if len(target.Selection.Branch) > MaxRepositoryViewSourceKeyBytes {
		return ErrMemoryProjectionLimit
	}
	return nil
}

func MemoryReadTargetBytes(ctx context.Context, target MemoryReadTarget) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMemoryReadTargetBytes {
		return nil, ErrMemoryProjectionLimit
	}
	return raw, ctx.Err()
}

func MemoryReadTargetID(ctx context.Context, target MemoryReadTarget) (ContentHash, error) {
	raw, err := MemoryReadTargetBytes(ctx, target)
	if err != nil {
		return "", err
	}
	return HashContent(append([]byte("cxthub:memory-read-target:v1\x00"), raw...)), nil
}

type MemoryReadWork struct {
	ID              ContentHash
	Target          MemoryReadTarget
	Source          RepositoryMemorySourceGeneration
	State           string
	Reason          string
	Attempts        int
	Version         int64
	QualificationID ContentHash
	CreatedAt       time.Time
	UpdatedAt       time.Time
	NextAttempt     time.Time
	LeaseUntil      time.Time
}

func (work MemoryReadWork) Validate() error {
	identifier, err := MemoryReadTargetID(context.Background(), work.Target)
	if err != nil || work.ID != identifier || work.Source.Validate() != nil || work.Source.Graph.RepoID != work.Target.RepoID ||
		work.Attempts < 0 || work.Attempts > MaxMemoryReadWorkAttempts || work.Version < 1 ||
		ValidateOptionalContentHash(work.QualificationID) != nil || work.CreatedAt.IsZero() || work.UpdatedAt.IsZero() || work.NextAttempt.IsZero() {
		return ErrIntegrity
	}
	switch work.State {
	case "waiting", "retrying", "rejected":
		if !work.LeaseUntil.IsZero() {
			return ErrIntegrity
		}
	case "running":
		if work.LeaseUntil.IsZero() || work.Attempts == 0 {
			return ErrIntegrity
		}
	case "completed":
		if !work.LeaseUntil.IsZero() {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	if !MemoryReadWorkReasonValid(work.Reason) {
		return ErrIntegrity
	}
	return nil
}

func MemoryReadWorkReasonValid(reason string) bool {
	switch reason {
	case "", "temporary_failure", "source_changed", "source_reset", "resource_limit", "invalid_source", "access_denied", "identity_upgrade_required", "retention_expired", "retention_cleanup", "attempts_exhausted", "comparison_mismatch":
		return true
	default:
		return false
	}
}
