package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	CatalogVersion      = 1
	CatalogScope        = "sync-metadata-v1"
	DefaultCatalogLimit = 256
	MaxCatalogLimit     = 1000
)

var (
	ErrCatalogUnsupported   = errors.New("catalog synchronization unsupported by this storage adapter")
	ErrCatalogResetRequired = errors.New("catalog synchronization requires a new baseline")
)

// CatalogCheckpoint identifies a complete committed metadata image, not verified
// document availability, graph publication, or applied client state.
type CatalogCheckpoint struct {
	Version  int         `json:"version"`
	RepoID   ContentHash `json:"repo_id"`
	Epoch    string      `json:"epoch"`
	Sequence int64       `json:"sequence"`
}

// CatalogRequest starts a baseline, starts a delta after a complete checkpoint,
// or resumes a page through an opaque adapter-owned cursor. Zero Limit uses 256.
type CatalogRequest struct {
	Version int                `json:"version"`
	After   *CatalogCheckpoint `json:"after,omitempty"`
	Cursor  string             `json:"cursor,omitempty"`
	Limit   int                `json:"limit,omitempty"`
}

// Validate checks the transport-independent shape. The store binds checkpoints
// and cursors to the requested repository, scope, epoch, and retention floor.
func (r CatalogRequest) Validate() error {
	if r.Version != CatalogVersion {
		return fmt.Errorf("%w: unsupported catalog request version", ErrValidation)
	}
	if r.Limit < 0 || r.Limit > MaxCatalogLimit {
		return fmt.Errorf("%w: catalog limit must be between 0 and %d", ErrValidation, MaxCatalogLimit)
	}
	if r.After != nil && r.Cursor != "" {
		return fmt.Errorf("%w: catalog after and cursor are mutually exclusive", ErrValidation)
	}
	if r.After != nil {
		if r.After.Version != CatalogVersion {
			return ErrCatalogResetRequired
		}
		if ValidateContentHash(r.After.RepoID) != nil || !catalogEpochValid(r.After.Epoch) || r.After.Sequence < 0 {
			return fmt.Errorf("%w: invalid catalog checkpoint", ErrValidation)
		}
	}
	return nil
}

func catalogEpochValid(epoch string) bool {
	if len(epoch) != 36 {
		return false
	}
	for i := range len(epoch) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if epoch[i] != '-' {
				return false
			}
			continue
		}
		c := epoch[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// CatalogEntry carries an original stored metadata image or a deletion. Ref
// keys encode [kind,name] as a JSON array string; other keys are entity/repo IDs.
// Value preserves raw lifecycle refs and complete history events. Consumers
// project refs under the recorded protocol before treating them as visible refs.
type CatalogEntry struct {
	Sequence int64           `json:"sequence"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Deleted  bool            `json:"deleted,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// CatalogPage is bounded at a fixed committed Through sequence. A partial page
// has NextCursor only; only the final page publishes Checkpoint. Pages may split
// a transaction, so clients must stage them until that final checkpoint arrives.
type CatalogPage struct {
	Version    int                `json:"version"`
	RepoID     ContentHash        `json:"repo_id"`
	Epoch      string             `json:"epoch"`
	Through    int64              `json:"through"`
	Mode       string             `json:"mode"`
	Entries    []CatalogEntry     `json:"entries"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Checkpoint *CatalogCheckpoint `json:"checkpoint,omitempty"`
}
