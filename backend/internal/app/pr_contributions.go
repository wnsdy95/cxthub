package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type verifiedPRContribution = evidencequery.VerifiedPRContribution

func contributionFromParticipation(proof domain.PRParticipation, checkpoint domain.PRHistoryCheckpoint, destination string) domain.PRContribution {
	return (domain.PRContribution{RepoID: proof.RepoID, GitOrigin: proof.GitOrigin, ProviderRepositoryID: proof.ProviderRepositoryID,
		PullRequestID: proof.PullRequestID, PR: checkpoint.PR, DestinationBranchID: destination, SourceBranchID: proof.BranchID,
		Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
}

func (s *Service) preparePRContributions(ctx context.Context, checkpoint domain.PRHistoryCheckpoint, proofs []domain.PRParticipation, history []domain.HistoryEvent) ([]domain.PRContribution, []domain.PRContributionMemory, error) {
	return s.prPublications().preparePRContributions(ctx, checkpoint, proofs, history)
}

func contributionInCode(ctx context.Context, evidence *codeEvidence, merge, code string) (bool, error) {
	if evidence == nil || code == "" {
		return false, nil
	}
	seen := map[string]bool{}
	for current := code; current != "" && len(seen) < maxCodeEvidenceReads; {
		if seen[current] {
			return false, domain.ErrIntegrity
		}
		seen[current] = true
		commit, err := evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, current)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if current == merge {
			return true, nil
		}
		if len(commit.Parents) == 0 {
			return false, nil
		}
		current = commit.Parents[0]
	}
	return false, nil
}

func (s *Service) verifyContributionClosure(ctx context.Context, record domain.PRContribution) error {
	return s.verifyContributionCapture(ctx, record.RepoID, record.Capture)
}

func (s *Service) verifyContributionCapture(ctx context.Context, repo domain.ContentHash, capture domain.ContentHash) error {
	return verifyContributionCaptureFromReader(ctx, s.meta, repo, capture)
}

func verifyContributionCaptureFromReader(ctx context.Context, reader outbound.MemoryMetadataReader, repo domain.ContentHash, capture domain.ContentHash) error {
	active, done := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if active[id] {
			return domain.ErrIntegrity
		}
		if done[id] {
			return nil
		}
		if len(done)+len(active) >= maxProjectionSnapshots {
			return domain.ErrIntegrity
		}
		snapshot, err := reader.GetSnapshot(ctx, repo, id)
		if err != nil {
			return err
		}
		if snapshot.RepoID != repo || snapshot.ID != id {
			return domain.ErrIntegrity
		}
		active[id] = true
		for _, parent := range snapshot.Parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		delete(active, id)
		done[id] = true
		return nil
	}
	return visit(capture)
}

func (s *Service) contributionDestination(ctx context.Context, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	return contributionDestinationFromReader(ctx, s.meta, repo, name, identity, history)
}

func contributionDestinationFromReader(ctx context.Context, reader outbound.MemoryMetadataReader, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	bindings, err := domain.ProjectContextBranches(history)
	if err != nil {
		return domain.Ref{}, err
	}
	name, err = contributionBranchName(repo, name, identity, bindings)
	if err != nil {
		return domain.Ref{}, err
	}
	ref, err := reader.GetRef(ctx, repo, domain.RefBranch, name)
	if err != nil {
		return ref, err
	}
	return bindContributionRef(ref, identity)
}

func contributionBranchName(repo domain.ContentHash, name, identity string, bindings domain.ContextBranchProjection) (string, error) {
	if binding, ok := bindings.ByID[identity]; ok {
		if binding.Archived {
			return "", domain.ErrConflict
		}
		name = binding.Name
	} else if bindings.Identity(string(repo), name) != identity || bindings.Released[name] != "" {
		return "", domain.ErrConflict
	}
	return name, nil
}

func bindContributionRef(ref domain.Ref, identity string) (domain.Ref, error) {
	if ref.BranchID != "" && ref.BranchID != identity {
		return ref, domain.ErrConflict
	}
	ref.BranchID = identity
	return ref, nil
}

func (service *Service) verifiedPRContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]verifiedPRContribution, error) {
	queries, err := service.evidenceQueries()
	if err != nil {
		return nil, err
	}
	return queries.VerifiedPR(ctx, repo, history)
}

func (s *PRPromotionWorker) promoteHistoricalPRContribution(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, bool, error) {
	var empty inbound.UpdateRefOutput
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return empty, false, err
	}
	records, err := s.verifiedPRContributions(ctx, repo, history)
	if err != nil {
		return empty, false, err
	}
	var candidates []domain.PRContribution
	applied := false
	for _, verified := range records {
		if verified.Record.PR != pr || (baseID != "" && verified.Record.DestinationBranchID != baseID) {
			continue
		}
		applied = true
		if verified.Valid {
			candidates = append(candidates, verified.Record)
		}
	}
	if len(candidates) == 0 {
		if applied {
			return empty, true, domain.ErrConflict
		}
		return empty, false, nil
	}
	selected := candidates[0]
	ref, err := s.contributionDestination(ctx, repo, pr.BaseBranch, selected.DestinationBranchID, history)
	if err != nil {
		return empty, true, err
	}
	out := inbound.UpdateRefOutput{Ref: ref, ServerTarget: ref.Target, RequestedTarget: selected.Capture, Result: inbound.RefUpToDate}
	return out, true, nil
}
