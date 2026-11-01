package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type frozenGitAdmission struct {
	snapshot    *gitAdmissionSnapshot
	input       domain.GitContributionAdmission
	source      domain.Ref
	destination domain.Ref
}

type gitAdmissionAuthorization struct {
	repo     domain.ContentHash
	provider domain.GitProviderObservation
	fence    GitReadFence
}

func authorizeGitAdmission(ctx context.Context, policy GitSourceAuthorizer, repo domain.ContentHash) (gitAdmissionAuthorization, error) {
	identity, fence, err := authorizeGitObservation(ctx, policy, repo)
	if err != nil {
		return gitAdmissionAuthorization{}, err
	}
	if identity == nil || fence == nil {
		return gitAdmissionAuthorization{}, domain.ErrForbidden
	}
	return gitAdmissionAuthorization{repo: repo, provider: *identity, fence: fence}, nil
}

func (workers *GitIntegrationWorkers) freezeGitAdmission(ctx context.Context, repo domain.ContentHash, publicationID, sourceHead, destinationName, destinationID string) (frozenGitAdmission, error) {
	return repositoryReadForRepoWithWork(ctx, workers.work, repo, func(read context.Context) (frozenGitAdmission, error) {
		snapshot, err := workers.readGitAdmissionEvidence(read, repo, []gitAdmissionLookup{{publicationID: publicationID, sourceHead: sourceHead}})
		if err != nil {
			return frozenGitAdmission{}, err
		}
		return snapshot.freeze(publicationID, sourceHead, destinationName, destinationID)
	})
}

func prepareGitAdmission(ctx context.Context, frozen frozenGitAdmission, receipt domain.GitHeadReceipt, evidence *codeEvidence) (domain.GitContributionAdmission, error) {
	var empty domain.GitContributionAdmission
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	input := frozen.input
	if receipt.Validate() != nil || receipt.RepoID != input.Destination.RepoID || receipt.GitOrigin != input.Destination.GitOrigin ||
		evidence == nil || evidence.repo != receipt.RepoID || evidence.origin != receipt.GitOrigin {
		return empty, domain.ErrIntegrity
	}
	input.Destination.InstallationID = receipt.Provider.InstallationID
	input.Destination.ProviderRepositoryID = receipt.Provider.RepositoryID
	input.Destination.ReadReceiptID, input.Destination.ReadVersion = receipt.ID, receipt.Version
	if source, found := receipt.SourceHead("refs/heads/" + input.SourceBranch); found && source.Commit == input.SourceHead {
		input.SourceObservationID = source.ObservationID
	}
	for _, head := range receipt.Heads {
		if head.Ref == "refs/heads/"+input.Destination.DestinationBranch {
			input.Destination.DestinationHead, input.Destination.ObservationID = head.Commit, head.ObservationID
		}
	}
	if input.SourceObservationID == "" || input.Destination.ObservationID == "" {
		return empty, domain.ErrNotFound
	}
	if _, err := locateGitContributionIntroduction(ctx, evidence, input.OriginalHead, input.SourceHead); err != nil {
		return empty, err
	}
	_, err := locateGitContributionIntroduction(ctx, evidence, input.SourceHead, input.Destination.DestinationHead)
	if err == nil {
		return empty, errGitContributionAlreadyPresent
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return empty, err
	}
	input.OrdinaryObservationIDs = append([]string{}, input.OrdinaryObservationIDs...)
	input.CarryObservationIDs = append([]string{}, input.CarryObservationIDs...)
	input.Destination = input.Destination.WithID()
	input.AfterReadVersion = receipt.Version
	input = input.WithID()
	if !input.MatchesBaseline(receipt) {
		return empty, domain.ErrIntegrity
	}
	return input, nil
}

func (workers *GitIntegrationWorkers) persistGitAdmission(ctx context.Context, frozen frozenGitAdmission, prepared domain.GitContributionAdmission, authorization gitAdmissionAuthorization) (domain.GitContributionAdmission, error) {
	return repositoryWriteWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), workers.work, frozen.input.Destination.RepoID, func(write context.Context) (domain.GitContributionAdmission, error) {
		return workers.persistGitAdmissionInWrite(write, frozen, prepared, authorization, nil)
	})
}

func (workers *GitIntegrationWorkers) persistGitAdmissionInWrite(write context.Context, frozen frozenGitAdmission, prepared domain.GitContributionAdmission, authorization gitAdmissionAuthorization, snapshot *gitAdmissionSnapshot) (domain.GitContributionAdmission, error) {
	var empty domain.GitContributionAdmission
	store, ok := workers.work.meta.(outbound.GitContributionAdmissionStore)
	if _, atomic := workers.work.meta.(outbound.RepositoryTransactions); !atomic || !ok || authorization.fence == nil || authorization.provider.Validate() != nil || prepared.Validate() != nil {
		return empty, domain.ErrValidation
	}
	repo := frozen.input.Destination.RepoID
	if prepared.Destination.RepoID != repo || !sameGitAdmissionInput(frozen.input, prepared) {
		return empty, domain.ErrIntegrity
	}
	if authorization.repo != repo || authorization.provider.InstallationID != prepared.Destination.InstallationID || authorization.provider.RepositoryID != prepared.Destination.ProviderRepositoryID {
		return empty, domain.ErrConflict
	}
	if err := authorization.fence(write); err != nil {
		return empty, err
	}
	prior, err := store.FindGitContributionAdmission(write, repo, prepared.Key())
	if err == nil {
		if prior.Validate() != nil || prior.Key() != prepared.Key() {
			return empty, domain.ErrIntegrity
		}
		return prior, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return empty, err
	}
	if snapshot == nil {
		snapshot, err = workers.reuseGitAdmissionSnapshot(write, repo, frozen.snapshot)
		if err != nil {
			return empty, err
		}
	}
	if snapshot.repo.ID != repo {
		return empty, domain.ErrIntegrity
	}
	current, err := snapshot.freeze(frozen.input.PublicationID, frozen.input.SourceHead, frozen.destination.Name, frozen.destination.BranchID)
	if err != nil {
		return empty, err
	}
	current.snapshot, frozen.snapshot = nil, nil
	if !reflect.DeepEqual(current, frozen) {
		return empty, domain.ErrConflict
	}
	if err := workers.verifyContributionCapture(write, repo, prepared.Capture); err != nil {
		return empty, err
	}
	watermark, err := store.FenceGitHeadRead(write, repo, prepared.Destination.GitOrigin)
	if err != nil {
		return empty, err
	}
	if watermark < prepared.Destination.ReadVersion {
		return empty, domain.ErrConflict
	}
	prepared.AfterReadVersion = watermark
	prepared = prepared.WithID()
	if err := store.PutGitContributionAdmission(write, prepared); err != nil {
		return empty, err
	}
	return prepared, nil
}

func sameGitAdmissionInput(input, prepared domain.GitContributionAdmission) bool {
	prepared.ID, prepared.SourceObservationID, prepared.AfterReadVersion = "", "", 0
	prepared.Destination.ID, prepared.Destination.ObservationID, prepared.Destination.ReadReceiptID = "", "", ""
	prepared.Destination.InstallationID, prepared.Destination.ProviderRepositoryID, prepared.Destination.ReadVersion = 0, 0, 0
	prepared.Destination.DestinationHead = ""
	return reflect.DeepEqual(input, prepared)
}
