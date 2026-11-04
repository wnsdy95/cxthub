package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type BoundedMemoryReader interface {
	GetMemoryForCapture(ctx context.Context, repo, hash domain.ContentHash, maxBytes int) (domain.MemoryDigest, error)
}

type WitnessedMemoryReader interface {
	CaptureMemoryObject(context.Context, domain.ContentHash, domain.ContentHash, int) (domain.CapturedMemoryObject, error)
	ValidateMemoryObjectWitnesses(context.Context, domain.ContentHash, []domain.MemoryObjectWitness) error
}

type MemoryCapturePinReader interface {
	ReadMemoryPinsForCapture(ctx context.Context, repo domain.ContentHash) ([]domain.PRContributionMemory, []domain.GitContributionMemory, error)
}
