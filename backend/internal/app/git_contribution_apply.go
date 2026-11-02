package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type preparedGitContribution struct {
	snapshot    *gitAdmissionSnapshot
	proof       domain.GitContributionProof
	admission   domain.GitContributionAdmission
	head        domain.GitContextHead
	receipt     domain.GitHeadReceipt
	publication domain.HistoryEvent
	memory      *domain.GitContributionMemory
	revision    domain.RepositoryRevision
}

func (snapshot *gitAdmissionSnapshot) admissionPublication(admission domain.GitContributionAdmission, head domain.GitContextHead) (domain.HistoryEvent, error) {
	publication, err := snapshot.admittedPublication(admission, head)
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	for owner := range snapshot.owners[admission.SourceHead] {
		if owner != admission.SourceBranchID {
			return domain.HistoryEvent{}, domain.ErrConflict
		}
	}
	return publication, nil
}

func (snapshot *gitAdmissionSnapshot) admittedPublication(admission domain.GitContributionAdmission, head domain.GitContextHead) (domain.HistoryEvent, error) {
	return domain.GitAdmittedPublication(snapshot.repo, snapshot.index, snapshot.history, admission, head)
}

func (workers *GitIntegrationWorkers) prepareGitContribution(ctx context.Context, admission domain.GitContributionAdmission, head domain.GitContextHead) (preparedGitContribution, error) {
	return repositoryReadForRepoWithWork(ctx, workers.work, admission.Destination.RepoID, func(read context.Context) (preparedGitContribution, error) {
		var prepared preparedGitContribution
		if admission.Validate() != nil || head.Validate() != nil {
			return prepared, domain.ErrValidation
		}
		admissions, ok := workers.work.meta.(outbound.GitContributionAdmissionStore)
		heads, headStore := workers.work.meta.(outbound.GitContextHeadStore)
		receipts, receiptStore := workers.work.meta.(outbound.GitHeadReceiptStore)
		if !ok || !headStore || !receiptStore {
			return prepared, domain.ErrValidation
		}
		stored, err := admissions.GetGitContributionAdmission(read, admission.Destination.RepoID, admission.ID)
		if err != nil {
			return prepared, err
		}
		if !reflect.DeepEqual(stored, admission) {
			return prepared, domain.ErrIntegrity
		}
		storedHead, err := heads.GetGitContextHead(read, admission.Destination.RepoID, head.ID)
		if err != nil {
			return prepared, err
		}
		if storedHead != head {
			return prepared, domain.ErrIntegrity
		}
		receipt, err := receipts.GetGitHeadReceipt(read, head.Destination.RepoID, head.Destination.GitOrigin, head.Destination.ReadVersion)
		if err != nil {
			return prepared, err
		}
		if !admission.MatchesLaterContextHead(head, receipt) {
			return prepared, domain.ErrConflict
		}
		baseline, err := receipts.GetGitHeadReceipt(read, admission.Destination.RepoID, admission.Destination.GitOrigin, admission.Destination.ReadVersion)
		if err != nil {
			return prepared, err
		}
		if !admission.MatchesBaseline(baseline) {
			return prepared, domain.ErrIntegrity
		}
		snapshot, err := workers.readGitAdmissionEvidence(read, admission.Destination.RepoID, []gitAdmissionLookup{historicalGitAdmissionLookup(admission)})
		if err != nil {
			return prepared, err
		}
		publication, err := snapshot.admissionPublication(admission, head)
		if err != nil {
			return prepared, err
		}
		evidence, err := workers.newCodeEvidence(read, admission.Destination.RepoID)
		if err != nil {
			return prepared, err
		}
		if _, err := locateGitContributionIntroduction(read, evidence, admission.OriginalHead, admission.SourceHead); err != nil {
			return prepared, err
		}
		integration, err := locateGitContributionInRange(read, evidence, admission.SourceHead, admission.Destination.DestinationHead, head.Destination.DestinationHead)
		if err != nil {
			return prepared, err
		}
		if err := workers.verifyContributionCapture(read, admission.Destination.RepoID, admission.Capture); err != nil {
			return prepared, err
		}
		record := (domain.GitContribution{RepoID: admission.Destination.RepoID, GitOrigin: admission.Destination.GitOrigin, ProviderRepositoryID: admission.Destination.ProviderRepositoryID,
			DestinationBranchID: admission.Destination.DestinationBranchID, SourceBranchID: admission.SourceBranchID, Capture: admission.Capture, OriginalHead: admission.OriginalHead, IntegrationSHA: integration}).WithID()
		proof := (domain.GitContributionProof{AdmissionID: admission.ID, ContextHeadID: head.ID, Contribution: record}).WithID()
		if !proof.MatchesInputs(admission, head, receipt) {
			return prepared, domain.ErrIntegrity
		}
		prepared = preparedGitContribution{proof: proof, admission: admission, head: head, receipt: receipt, publication: publication, revision: snapshot.revision, snapshot: snapshot}
		if publication.MemoryPinned {
			source, err := workers.contributionPublicationMemorySource(read, record.RepoID, record.Capture, publication)
			if err != nil {
				return preparedGitContribution{}, err
			}
			memory := (domain.GitContributionMemory{RepoID: record.RepoID, ContributionID: record.ID, PublicationID: publication.ID, MemoryHash: publication.MemoryHash, MemorySource: source, MemoryPinned: true}).WithID()
			if memory.Validate() != nil {
				return preparedGitContribution{}, domain.ErrIntegrity
			}
			prepared.memory = &memory
		}
		return prepared, nil
	})
}

func (workers *GitIntegrationWorkers) applyGitContribution(ctx context.Context, prepared preparedGitContribution, authorization gitAdmissionAuthorization) (domain.GitContributionProof, error) {
	var empty domain.GitContributionProof
	repo := prepared.proof.Contribution.RepoID
	proofs, ok := workers.work.meta.(outbound.GitContextHeadStore)
	ledger, hasLedger := workers.work.meta.(outbound.GitContributionStore)
	_, atomic := workers.work.meta.(outbound.RepositoryTransactions)
	if !ok || !hasLedger || !atomic || authorization.fence == nil || !prepared.proof.MatchesInputs(prepared.admission, prepared.head, prepared.receipt) {
		return empty, domain.ErrValidation
	}
	if authorization.repo != repo || authorization.provider != prepared.receipt.Provider {
		return empty, domain.ErrConflict
	}
	return repositoryWriteWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), workers.work, repo, func(write context.Context) (domain.GitContributionProof, error) {
		if err := authorization.fence(write); err != nil {
			return empty, err
		}
		current, err := workers.workerRepository(write, repo)
		if err != nil {
			return empty, err
		}
		if current.GitRemoteURL != prepared.proof.Contribution.GitOrigin {
			return empty, domain.ErrConflict
		}
		prior, err := proofs.FindGitContributionProof(write, repo, prepared.admission.ID)
		if err == nil {
			if prior.Contribution != prepared.proof.Contribution {
				return empty, domain.ErrConflict
			}
			return prior, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return empty, err
		}
		snapshot, err := workers.reuseGitAdmissionSnapshot(write, repo, prepared.snapshot)
		if err != nil {
			return empty, err
		}
		if snapshot.revision.Graph != prepared.revision.Graph || snapshot.revision.Evidence != prepared.revision.Evidence {
			return empty, domain.ErrConflict
		}
		publication, err := snapshot.admissionPublication(prepared.admission, prepared.head)
		if err != nil {
			return empty, err
		}
		if !reflect.DeepEqual(publication, prepared.publication) {
			return empty, domain.ErrConflict
		}
		if prepared.memory != nil {
			source, err := workers.contributionPublicationMemorySource(write, repo, prepared.proof.Contribution.Capture, publication)
			if err != nil {
				return empty, err
			}
			want := (domain.GitContributionMemory{RepoID: repo, ContributionID: prepared.proof.Contribution.ID, PublicationID: publication.ID, MemoryHash: publication.MemoryHash, MemorySource: source, MemoryPinned: publication.MemoryPinned}).WithID()
			if want != *prepared.memory {
				return empty, domain.ErrIntegrity
			}
		} else if publication.MemoryPinned {
			return empty, domain.ErrIntegrity
		}
		if err := ledger.PutGitContribution(write, prepared.proof.Contribution); err != nil {
			return empty, err
		}
		if prepared.memory != nil {
			if err := ledger.PutGitContributionMemory(write, *prepared.memory); err != nil {
				return empty, err
			}
		}
		if err := proofs.PutGitContributionProof(write, prepared.proof); err != nil {
			return empty, err
		}
		if err := workers.work.revisionWrite(evidenceWriteContext(write), repo); err != nil {
			return empty, err
		}
		return prepared.proof, nil
	})
}
