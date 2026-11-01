package domain

import "errors"

const GraphProjectionRules = 1

const GraphViewProjectionRules = 2

var ErrGraphProjectionNotReady = errors.New("graph projection is not ready for the current source")

var ErrGraphProjectionFailed = errors.New("graph projection requires attention")

type GraphProjectionLease PendingProjectionLease

func (lease GraphProjectionLease) SourceRequest() RepositoryViewSourceRequest {
	request := PendingProjectionLease(lease).SourceRequest()
	if request.After != nil {
		request.After.Scope = RepositoryGraphSourceScope
	}
	return request
}

type GraphProjectionPart = PendingProjectionPart

type GraphProjectionRead struct {
	NeedsAttention bool
	Enabled        bool
	Ready          bool
	Complete       bool
	Rules          int
	Dataset        string
	Source         RepositoryViewSourceCheckpoint
	Applied        RepositoryViewSourceCheckpoint
	Parts          []GraphProjectionPart
	Qualification  *GraphQualificationResult
	ServingFence   int64
}

type GraphReadGeneration struct {
	Generation   ContentHash `json:"generation"`
	SourceEpoch  string      `json:"source_epoch"`
	AppliedEpoch string      `json:"applied_epoch"`
	Rules        int         `json:"rules"`
	Complete     bool        `json:"complete"`
	Current      bool        `json:"current"`
}
