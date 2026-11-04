package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// VerifyGitReversal reads evidence outside a database write transaction. Its
// result is a verified immutable commit relation, not a shared branch movement.
// A caller recording it must recheck branch identity/origin in its transaction.
func (s *Service) VerifyGitReversal(ctx context.Context, reader outbound.GitEvidenceReader, repoID domain.ContentHash, target, targetParent, candidate, candidateParent string) (domain.GitReversalEvidence, error) {
	var empty domain.GitReversalEvidence
	if err := domain.ValidateContentHash(repoID); err != nil {
		return empty, err
	}
	for _, oid := range []string{target, candidate} {
		if err := domain.ValidateGitOID(oid); err != nil {
			return empty, err
		}
	}
	for _, oid := range []string{targetParent, candidateParent} {
		if oid != "" {
			if err := domain.ValidateGitOID(oid); err != nil {
				return empty, err
			}
		}
	}
	if reader == nil {
		return empty, fmt.Errorf("Git evidence reader unavailable")
	}
	repo, err := repositoryReadForRepo(ctx, s, repoID, func(bound context.Context) (domain.Repo, error) { return s.meta.GetRepo(bound, repoID) })
	if err != nil {
		return empty, err
	}
	if repo.GitRemoteURL == "" {
		return empty, fmt.Errorf("%w: repository has no verified Git origin", domain.ErrValidation)
	}
	return verifyGitReversal(ctx, reader, repo.GitRemoteURL, target, targetParent, candidate, candidateParent)
}

func verifyGitReversal(ctx context.Context, reader outbound.GitEvidenceReader, origin, target, targetParent, candidate, candidateParent string) (domain.GitReversalEvidence, error) {
	return evidencequery.VerifyReversal(ctx, reader, origin, target, targetParent, candidate, candidateParent)
}
