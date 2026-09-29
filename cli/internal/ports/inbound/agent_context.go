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
