package domain

import "time"

// SnapshotBackfill is a durable obligation to publish a retained snapshot and
// its dependencies. A retry never changes a branch, pending pointer or history
// event. StateHash identifies the requested mutable attachment projection, not
// the immutable document identity. Version fences delayed worker acknowledgments.
type SnapshotBackfill struct {
	RepoID      string      `json:"repo_id"`
	Snapshot    ContentHash `json:"snapshot"`
	StateHash   ContentHash `json:"state_hash"`
	Version     uint64      `json:"version"`
	CreatedAt   time.Time   `json:"created_at"`
	NextAttempt time.Time   `json:"next_attempt"`
	Attempts    uint32      `json:"attempts"`
	Reason      string      `json:"reason,omitempty"`
}

func (j SnapshotBackfill) Validate() error {
	for _, h := range []ContentHash{ContentHash(j.RepoID), j.Snapshot, j.StateHash} {
		if err := ValidateContentHash(h); err != nil {
			return err
		}
	}
	if j.Version == 0 || j.CreatedAt.IsZero() || j.NextAttempt.IsZero() {
		return ErrHashMismatch
	}
	switch j.Reason {
	case "", "unavailable", "integrity", "conflict", "cancelled":
	default:
		return ErrHashMismatch
	}
	return nil
}
