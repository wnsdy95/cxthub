package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// MemoryReuse copies an exact immutable body, not its ownership or causal
// attachment. Version 1 preserves all fields except target, parent and provider.
type MemoryReuse struct {
	Version            uint32              `json:"version"`
	BaseHash           domain.ContentHash  `json:"base_hash"`
	MemoryHash         domain.ContentHash  `json:"memory_hash"`
	PreviousMemoryHash domain.ContentHash  `json:"previous_memory_hash,omitempty"`
	Provider           domain.ProviderKind `json:"provider"`
}

type MemoryReuser interface {
	ReuseMemoryDigest(context.Context, domain.ContentHash, domain.ContentHash, MemoryReuse) (domain.ContentHash, error)
}
