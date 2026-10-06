package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryInitializationStore = (*FSStore)(nil)

// FS does not serialize access-policy changes with graph writes or provide a
// multi-file repository transaction. Never turn that into weaker onboarding.
func (*FSStore) GetRepositoryInitialization(context.Context, domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationUnsupported
}
func (*FSStore) BeginRepositoryInitialization(context.Context, domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationUnsupported
}
func (*FSStore) FinalizeRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationFinalize, outbound.RepositoryInitializationProof) (domain.RepositoryInitializationReceipt, error) {
	return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationUnsupported
}

func (*FSStore) CaptureRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationAnchor, outbound.RepositoryInitializationEvidence) (outbound.RepositoryInitializationProof, error) {
	return nil, domain.ErrRepositoryInitializationUnsupported
}

func (*FSStore) GetRepositoryInitializationAnchor(context.Context, domain.ContentHash, string) (domain.RepositoryInitializationAnchor, error) {
	return domain.RepositoryInitializationAnchor{}, domain.ErrRepositoryInitializationUnsupported
}
