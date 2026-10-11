package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type gitContributionCandidate struct {
	publication          domain.HistoryEvent
	witness              domain.GitDestinationCandidate
	sourceObservations   []string
	carryObservations    []string
	sourceHead           string
	possibleIntroduction string
	memorySource         domain.ContentHash
}

func (s *Service) inspectGitDestinationCandidate(ctx context.Context, receipt domain.GitHeadReceipt, observation domain.GitRefObservation, ref domain.Ref, history []domain.HistoryEvent) (domain.GitDestinationCandidate, error) {
	var empty domain.GitDestinationCandidate
	if receipt.Validate() != nil || observation.Validate() != nil || observation.Source != "reconciliation" || observation.Provider == nil || ref.RepoID != observation.RepoID || ref.Kind != domain.RefBranch || observation.Ref != "refs/heads/"+ref.Name || ref.BranchID == "" {
		return empty, domain.ErrIntegrity
	}
	repo, err := s.meta.GetRepo(ctx, ref.RepoID)
	if err != nil {
		return empty, err
	}
	if repo.GitRemoteURL != observation.GitOrigin {
		return empty, domain.ErrConflict
	}
	current, err := s.contributionDestination(ctx, ref.RepoID, ref.Name, ref.BranchID, history)
	if err != nil {
		return empty, err
	}
	if current != ref {
		return empty, domain.ErrConflict
	}
	witness := (domain.GitDestinationCandidate{RepoID: ref.RepoID, GitOrigin: observation.GitOrigin, InstallationID: observation.Provider.InstallationID,
		ProviderRepositoryID: observation.Provider.RepositoryID, ObservationID: observation.ID, DestinationBranchID: ref.BranchID,
		ReadReceiptID: receipt.ID, ReadVersion: receipt.Version, DestinationBranch: ref.Name, DestinationHead: observation.After}).WithID()
	if !witness.MatchesObservation(observation) || !witness.MatchesReceipt(receipt) {
		return empty, domain.ErrIntegrity
	}
	return witness, nil
}

func (s *Service) inspectGitContributionCandidate(ctx context.Context, witness domain.GitDestinationCandidate, publicationID, sourceHead string, publications *domain.GitPublicationIndex, evidence *codeEvidence) (gitContributionCandidate, error) {
	var result gitContributionCandidate
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if witness.Validate() != nil || evidence == nil || evidence.repo != witness.RepoID || normalizeGitURL(evidence.origin) != normalizeGitURL(witness.GitOrigin) {
		return result, domain.ErrIntegrity
	}
	publication, err := publications.Publication(publicationID)
	if err != nil {
		return result, err
	}
	if publication.RepoID != string(witness.RepoID) {
		return result, domain.ErrIntegrity
	}
	carry, err := publications.CarryObservationIDs(publication.ID, sourceHead)
	if err != nil {
		return result, err
	}
	if _, err := locateGitContributionIntroduction(ctx, evidence, publication.GitAfter, sourceHead); err != nil {
		return result, err
	}
	integration, err := locateGitContributionIntroduction(ctx, evidence, sourceHead, witness.DestinationHead)
	if err != nil {
		return result, err
	}
	if err := s.verifyContributionCapture(ctx, witness.RepoID, publication.Target); err != nil {
		return result, err
	}
	result.publication, result.witness, result.possibleIntroduction = publication, witness, integration
	result.sourceObservations = publications.ObservationIDs(publication.ID)
	result.carryObservations, result.sourceHead = carry, sourceHead
	if publication.MemoryPinned {
		source, err := s.contributionPublicationMemorySource(ctx, witness.RepoID, publication.Target, publication)
		if err != nil {
			return gitContributionCandidate{}, err
		}
		result.memorySource = source
	}
	return result, nil
}
