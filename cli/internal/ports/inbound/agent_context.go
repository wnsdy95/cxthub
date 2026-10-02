package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type PrepareAgentContextInput struct {
	RepoID     string
	Cwd        string
	Branch     string
	SnapshotID domain.ContentHash
	// LatestMain selects the current authorized server main for injection, not
	// the local working cursor. Explicit archive inspection may leave it false.
	LatestMain bool
	// WorkingPosition is independent of the injected project's source position.
	WorkingPosition *domain.AgentWorkingPosition
	// MemoryPin is an exact historical attachment; non-nil empty means no memory.
	MemoryPin *domain.AgentMemoryPin
	// WorktreeStateHash fences the runtime's selected cursor before delivery.
	WorktreeStateHash domain.ContentHash
	Provider          domain.ProviderKind
	Model             string
	Policy            domain.InputPolicy
	PersonalScope     domain.PersonalWorkScope
	// WorkStatePath explicitly selects a structured handoff artifact. The runtime
	// authenticates its principal and validates all provenance before consumption.
	WorkStatePath string
	// NativeResume never prepares or injects a package into an existing session.
	NativeResume bool
	// ArtifactOnly prepares inspectable data without permission to launch it.
	// Unknown host/tokenizer capability stays visible in the receipt.
	ArtifactOnly bool
}

type PrepareAgentContext interface {
	PrepareAgentContext(context.Context, PrepareAgentContextInput) (domain.AgentContextPackage, error)
}

// AgentContextDeliveryValidator rechecks source authorization and working state
// immediately before a prepared package is materialized or delivered.
type AgentContextDeliveryValidator interface {
	ValidateAgentContextDelivery(context.Context, string, domain.AgentContextSelection) error
}
