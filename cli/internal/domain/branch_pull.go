package domain

import "fmt"

const BranchPullVersion = 1
const MaxBranchPullRoots = 256

// BranchPullRequest selects evidence, not a local adoption or a branch ACL.
type BranchPullRequest struct {
	Version          int           `json:"version"`
	Branch           string        `json:"branch"`
	ObservationRoots []ContentHash `json:"observation_roots,omitempty"`
}

func (r BranchPullRequest) Validate() error {
	if r.Version != BranchPullVersion || ValidateBranchName(r.Branch) != nil || len(r.ObservationRoots) > MaxBranchPullRoots {
		return fmt.Errorf("%w: invalid branch pull request", ErrInvalidRef)
	}
	for _, id := range r.ObservationRoots {
		if ValidateContentHash(id) != nil {
			return fmt.Errorf("%w: invalid observation root", ErrInvalidRef)
		}
	}
	return nil
}

// BranchPullPlan describes a complete dependency inventory. State tokens verify
// subsequent metadata reads; this response is not a lease or an apply receipt.
// Memory roots come from frozen snapshot metadata and history; the receiver
// verifies actual digest owners and complete ancestry before accepting a fetch.
type BranchPullPlan struct {
	Version         int                         `json:"version"`
	RepoID          string                      `json:"repo_id"`
	Branch          string                      `json:"branch"`
	ContextProtocol int                         `json:"context_protocol"`
	SelectedRef     Ref                         `json:"selected_ref"`
	Refs            []Ref                       `json:"refs"`
	History         []HistoryEvent              `json:"history"`
	SnapshotIndex   []ContentHash               `json:"snapshot_index"`
	SnapshotStates  map[ContentHash]ContentHash `json:"snapshot_states"`
	AbsentRoots     []ContentHash               `json:"absent_roots"`
	SettingsObjects []BranchPullSettings        `json:"settings_objects"`
}

type BranchPullSettings struct {
	Kind string      `json:"kind"`
	Hash ContentHash `json:"hash"`
}
