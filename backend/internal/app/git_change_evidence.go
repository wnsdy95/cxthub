package app

import (
	"context"
	"fmt"
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
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return empty, err
	}
	if repo.GitRemoteURL == "" {
		return empty, fmt.Errorf("%w: repository has no verified Git origin", domain.ErrValidation)
	}
	return verifyGitReversal(ctx, reader, repo.GitRemoteURL, target, targetParent, candidate, candidateParent)
}

func verifyGitReversal(ctx context.Context, reader outbound.GitEvidenceReader, origin, target, targetParent, candidate, candidateParent string) (domain.GitReversalEvidence, error) {
	var empty domain.GitReversalEvidence
	before, err := reader.ReadCommitDelta(ctx, origin, target, targetParent)
	if err != nil {
		return empty, err
	}
	after, err := reader.ReadCommitDelta(ctx, origin, candidate, candidateParent)
	if err != nil {
		return empty, err
	}
	// A provider response for another immutable object is never accepted, even
	// when its bytes happen to form a plausible inverse.
	if before.Commit != target || after.Commit != candidate || (targetParent != "" && before.Parent != targetParent) || (candidateParent != "" && after.Parent != candidateParent) {
		return empty, domain.ErrIntegrity
	}
	if err := before.Validate(); err != nil {
		return empty, err
	}
	if err := after.Validate(); err != nil {
		return empty, err
	}
	if after.Parent == "" {
		return domain.AssessGitReversal(before, after)
	}
	ancestor, err := reader.IsGitAncestor(ctx, origin, target, after.Parent)
	if err != nil {
		return empty, err
	}
	if !ancestor {
		return domain.GitReversalEvidence{Commit: candidate, Target: target, Parent: after.Parent, TargetParent: before.Parent, Coverage: "unverified", Reason: "target_not_in_candidate_ancestry", Paths: []string{}, UnverifiedPaths: []string{}, OtherPaths: []string{}}, nil
	}
	return domain.AssessGitReversal(before, after)
}
