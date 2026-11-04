package domain

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	PendingProjectionRules          = 1
	MaxPendingProjectionInputBytes  = 32 << 20
	MaxPendingProjectionInputRows   = 100000
	MaxPendingProjectionOutputBytes = 4 << 20
	MaxPendingProjectionOutputRows  = 20000
)

var ErrPendingProjectionNotReady = errors.New("pending projection is not ready for the current source")

var ErrPendingProjectionFailed = errors.New("pending projection requires attention")

type PendingProjectionLease struct {
	RepoID          ContentHash
	Dataset         string
	Epoch           string
	Fence           int64
	Phase           string
	AppliedSequence int64
	InputSequence   int64
	Cursor          string
	LeaseUntil      time.Time
	Attempts        int
}

func (lease PendingProjectionLease) SourceRequest() RepositoryViewSourceRequest {
	request := RepositoryViewSourceRequest{Version: RepositoryViewSourceVersion, Cursor: lease.Cursor}
	if lease.Cursor == "" && lease.AppliedSequence >= 0 {
		request.After = &RepositoryViewSourceCheckpoint{Version: RepositoryViewSourceVersion, Scope: RepositoryViewSourceScope, RepoID: lease.RepoID, Epoch: lease.Epoch, Sequence: lease.AppliedSequence}
	}
	return request
}

type PendingProjectionPart struct {
	Kind  string          `json:"kind"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type PendingProjectionRead struct {
	NeedsAttention bool
	Enabled        bool
	Ready          bool
	Dataset        string
	Source         RepositoryViewSourceCheckpoint
	Applied        RepositoryViewSourceCheckpoint
	Parts          []PendingProjectionPart
}
