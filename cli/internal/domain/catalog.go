package domain

import (
	"encoding/json"
	"errors"
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

// CatalogCheckpoint identifies a complete acquired metadata image. It is not
// proof of document availability, graph publication, or applied client state.
type CatalogCheckpoint struct {
	Version  int    `json:"version"`
	RepoID   string `json:"repo_id"`
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

// CatalogRequest starts a baseline, a delta, or an opaque cursor continuation.
type CatalogRequest struct {
	Version int                `json:"version"`
	After   *CatalogCheckpoint `json:"after,omitempty"`
	Cursor  string             `json:"cursor,omitempty"`
	Limit   int                `json:"limit,omitempty"`
}

// CatalogEntry preserves the original stored metadata, including raw lifecycle
// refs and full history events. A deletion has no Value, including no JSON null.
type CatalogEntry struct {
	Sequence int64           `json:"sequence"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Deleted  bool            `json:"deleted,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// CatalogPage is bounded by one committed Through sequence. A partial page has
// NextCursor only; only the final page has Checkpoint. A transaction may span
// pages, so callers must stage the whole run before publishing its checkpoint.
type CatalogPage struct {
	Version    int                `json:"version"`
	RepoID     string             `json:"repo_id"`
	Epoch      string             `json:"epoch"`
	Through    int64              `json:"through"`
	Mode       string             `json:"mode"`
	Entries    []CatalogEntry     `json:"entries"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Checkpoint *CatalogCheckpoint `json:"checkpoint,omitempty"`
}
