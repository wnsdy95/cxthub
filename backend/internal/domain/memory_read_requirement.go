package domain

import "context"

const MemoryReadRequirementVersion = 1

const MaxPreparedMemoryCursorBytes = 4096

type MemoryReadPending struct {
	Cursor string
}

func (pending *MemoryReadPending) Error() string { return ErrMemoryReadNotReady.Error() }

func (pending *MemoryReadPending) Unwrap() error { return ErrMemoryReadNotReady }

func IsOnlyMemoryReadPending(err error) bool {
	if err == ErrMemoryReadNotReady {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsOnlyMemoryReadPending(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsOnlyMemoryReadPending(wrapped.Unwrap())
	default:
		return false
	}
}

func CheckMemorySourceMinimum(applied, minimum RepositoryMemorySourceGeneration) error {
	if err := applied.Validate(); err != nil {
		return err
	}
	if err := minimum.Validate(); err != nil {
		return err
	}
	if applied.Version != minimum.Version || applied.Graph.Version != minimum.Graph.Version ||
		applied.Graph.Scope != minimum.Graph.Scope || applied.Graph.RepoID != minimum.Graph.RepoID {
		return ErrValidation
	}
	if applied.Graph.Epoch != minimum.Graph.Epoch || applied.Epoch != minimum.Epoch {
		return ErrCatalogResetRequired
	}
	if applied.Graph.Sequence < minimum.Graph.Sequence || applied.Sequence < minimum.Sequence {
		return ErrMemoryReadNotReady
	}
	return nil
}

type MemoryReadRequirement struct {
	Version  int                              `json:"version"`
	TargetID ContentHash                      `json:"target_id"`
	Minimum  RepositoryMemorySourceGeneration `json:"minimum_epoch"`
}

func (requirement MemoryReadRequirement) Validate(target MemoryReadTarget) error {
	if requirement.Version != MemoryReadRequirementVersion || ValidateContentHash(requirement.TargetID) != nil {
		return ErrValidation
	}
	identifier, err := MemoryReadTargetID(context.Background(), target)
	if err != nil || identifier != requirement.TargetID {
		return ErrValidation
	}
	if err := requirement.Minimum.Validate(); err != nil {
		return err
	}
	if requirement.Minimum.Graph.RepoID != target.RepoID {
		return ErrValidation
	}
	return nil
}

type MemoryReadStatus struct {
	Version    int                              `json:"schema_version"`
	Rules      int                              `json:"algorithm_version"`
	Generation ContentHash                      `json:"generation"`
	Minimum    RepositoryMemorySourceGeneration `json:"minimum_epoch"`
	Observed   RepositoryMemorySourceGeneration `json:"source_epoch"`
	Applied    RepositoryMemorySourceGeneration `json:"applied_epoch"`
	Complete   bool                             `json:"complete"`
	Freshness  string                           `json:"freshness"`
}

func (status MemoryReadStatus) Validate() error {
	if status.Version != MemoryReadVersion || status.Rules != MemoryReadRules ||
		ValidateContentHash(status.Generation) != nil || !status.Complete {
		return ErrValidation
	}
	if err := CheckMemorySourceMinimum(status.Applied, status.Minimum); err != nil {
		return err
	}
	if err := CheckMemorySourceMinimum(status.Observed, status.Applied); err != nil {
		return err
	}
	freshness := "current"
	if status.Observed != status.Applied {
		freshness = "catching_up"
	}
	if status.Freshness != freshness {
		return ErrValidation
	}
	return nil
}

type MemoryReadPage struct {
	Cursor string              `json:"cursor,omitempty"`
	Status MemoryReadStatus    `json:"status"`
	Page   EffectiveMemoryPage `json:"page"`
}
