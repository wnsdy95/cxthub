package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Creation/finalization require the same store's repository write transaction.
// Body verification and capture use one read snapshot before finalization.
// Proof is operation-private, short-lived evidence, never persisted or on wire.
type RepositoryInitializationEvidence struct {
	Snapshots []domain.Snapshot
	Memories  []domain.ContentHash
}
type RepositoryInitializationProof interface{ RepositoryInitializationProof() }

type RepositoryInitializationStore interface {
	GetRepositoryInitialization(context.Context, domain.ContentHash) (domain.RepositoryInitializationReceipt, error)
	GetRepositoryInitializationAnchor(context.Context, domain.ContentHash, string) (domain.RepositoryInitializationAnchor, error)
	BeginRepositoryInitialization(context.Context, domain.Repo) (domain.RepositoryInitializationReceipt, error)
	CaptureRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationAnchor, RepositoryInitializationEvidence) (RepositoryInitializationProof, error)
	FinalizeRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationFinalize, RepositoryInitializationProof) (domain.RepositoryInitializationReceipt, error)
}
