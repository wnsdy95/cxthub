package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// WorkingMemoryCommit fixes the selection before distillation. Nil position
// means attachment-only, never permission to discover and repin a later cursor.
// A nonnil position is compared in full even when it is not eligible for repin.
type WorkingMemoryCommit struct {
	RepoID           string                  `json:"repo_id"`
	Snapshot         domain.ContentHash      `json:"snapshot"`
	ExpectedMemory   domain.ContentHash      `json:"expected_memory,omitempty"`
	Memory           domain.ContentHash      `json:"memory"`
	ExpectedPosition *domain.WorkingPosition `json:"expected_position,omitempty"`
}

type WorkingMemoryStore interface {
	CommitWorkingMemory(context.Context, WorkingMemoryCommit) error
}

type WorkingMemoryPins interface {
	HasWorkingMemoryPin(context.Context, domain.ContentHash) (bool, error)
}
