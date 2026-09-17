package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// RepositoryTransactions binds every store call made with the callback context
// to one database transaction. The callback must not start goroutines or make
// external calls. Repository writers serialize before reading mutable state.
// FS development storage deliberately does not claim this capability.
type RepositoryTransactions interface {
	WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinReadSnapshot(context.Context, func(context.Context) error) error
}

// PRJobFence holds the current job row until the repository transaction ends.
// A stale/expired worker must fail before changing any context state.
type PRJobFence interface {
	FencePRJob(context.Context, domain.PRPromotionJob) error
}

// SecretsCASStore keeps the validated encrypted envelope from being replaced
// between the HTTP consistency check and its write. nil means absent.
type SecretsCASStore interface {
	CompareAndSwapSecrets(context.Context, domain.ContentHash, []byte, []byte) error
}
