package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type StageSession struct {
	Provider  domain.ProviderKind
	Path      string
	SessionID string
}
type StageInput struct {
	// DocIdentity explicitly selects local capture identity; empty uses local policy, defaulting to legacy.
	DocIdentity      domain.DocumentIdentity
	Cwd              string
	Sessions         []StageSession
	ExpectedRevision domain.ContentHash
}
type UnstageInput struct {
	Cwd              string
	Keys             []domain.ContentHash
	All              bool
	ExpectedRevision domain.ContentHash
}
type StagingCommitInput struct {
	Cwd              string
	Message          string
	Author           domain.TeamIdentity
	ExpectedRevision domain.ContentHash
}

// Staging captures once at add; Commit never opens a provider transcript.
type Staging interface {
	Stage(context.Context, StageInput) (domain.StagingIndex, error)
	Inspect(context.Context, string) (domain.StagingIndex, error)
	Unstage(context.Context, UnstageInput) (domain.StagingIndex, error)
	Commit(context.Context, StagingCommitInput) (domain.StagingCommit, error)
	ResumeCommit(context.Context, string, string) (domain.StagingCommit, error)
}

type StagingStash interface {
	StashIndex(context.Context, string, domain.ContentHash) (domain.StagingStash, error)
	PopIndex(context.Context, string, string, domain.ContentHash) (domain.StagingIndex, error)
	ListIndexStashes(context.Context, string) ([]domain.StagingStash, error)
}
