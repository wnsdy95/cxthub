package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func ProjectCapturedRepositoryView(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, view domain.RepositoryView, evidence outbound.GraphEvidenceReader) (domain.RepositoryView, error) {
	return repositoryquery.ProjectCapturedRepositoryView(ctx, checkpoint, view, evidence, &legacyGraphProjectionScope{})
}

func (s *Service) graphEvidenceReader() outbound.RepositoryReader {
	if s.projectionEvidence != nil {
		return s.projectionEvidence
	}
	return s.meta
}

func ProjectCapturedGraph(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, view domain.RepositoryView, evidence outbound.GraphEvidenceReader, position string) (domain.GraphState, error) {
	return repositoryquery.ProjectCapturedGraph(ctx, checkpoint, view, evidence, position, &legacyGraphProjectionScope{})
}

func validateCapturedGraphScope(ctx context.Context, repo domain.ContentHash, view domain.RepositoryView) error {
	return domain.ValidateRepositoryViewScope(ctx, repo, view)
}
