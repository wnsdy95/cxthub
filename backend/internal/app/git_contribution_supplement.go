package app

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type preparedGitContributionSupplement struct {
	snapshot    *gitAdmissionSnapshot
	job         domain.GitContributionSupplement
	proof       domain.GitContributionProof
	admission   domain.GitContributionAdmission
	head        domain.GitContextHead
	publication domain.HistoryEvent
	memory      domain.GitContributionMemory
	revision    domain.RepositoryRevision
}

func gitSupplementPublication(index *domain.GitPublicationIndex, record domain.GitContribution, id string) (domain.HistoryEvent, error) {
	return domain.GitSupplementPublication(index, record, id)
}

func (workers *GitIntegrationWorkers) prepareGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) (preparedGitContributionSupplement, error) {
	return repositoryReadForRepoWithWork(ctx, workers.work, job.RepoID, func(read context.Context) (preparedGitContributionSupplement, error) {
		var prepared preparedGitContributionSupplement
		if job.Validate() != nil || job.State != "processing" {
			return prepared, domain.ErrValidation
		}
		store, supported := workers.work.meta.(outbound.GitContributionSupplementStore)
		admissions, admitted := workers.work.meta.(outbound.GitContributionAdmissionStore)
		heads, bound := workers.work.meta.(outbound.GitContextHeadStore)
		receipts, readReceipts := workers.work.meta.(outbound.GitHeadReceiptStore)
		if !supported || !admitted || !bound || !readReceipts {
			return prepared, domain.ErrValidation
		}
		proof, err := store.GetGitContributionSupplementProof(read, job.RepoID, job.ProofID)
		if err != nil {
			return prepared, err
		}
		record := proof.Contribution
		if proof.ID != job.ProofID || record.ID != job.ContributionID || record.RepoID != job.RepoID || record.GitOrigin != job.GitOrigin {
			return prepared, domain.ErrIntegrity
		}
		admission, err := admissions.GetGitContributionAdmission(read, job.RepoID, proof.AdmissionID)
		if err != nil {
			return prepared, err
		}
		head, err := heads.GetGitContextHead(read, job.RepoID, proof.ContextHeadID)
		if err != nil {
			return prepared, err
		}
		receipt, err := receipts.GetGitHeadReceipt(read, job.RepoID, job.GitOrigin, head.Destination.ReadVersion)
		if err != nil {
			return prepared, err
		}
		if !proof.MatchesInputs(admission, head, receipt) {
			return prepared, domain.ErrIntegrity
		}
		baseline, err := receipts.GetGitHeadReceipt(read, job.RepoID, job.GitOrigin, admission.Destination.ReadVersion)
		if err != nil {
			return prepared, err
		}
		if !admission.MatchesBaseline(baseline) {
			return prepared, domain.ErrIntegrity
		}
		snapshot, err := workers.readGitAdmissionEvidence(read, job.RepoID, []gitAdmissionLookup{historicalGitAdmissionLookup(admission), {publicationID: job.PublicationID}})
		if err != nil {
			return prepared, err
		}
		if _, err := snapshot.admittedPublication(admission, head); err != nil {
			return prepared, err
		}
		publication, err := gitSupplementPublication(snapshot.index, record, job.PublicationID)
		if err != nil {
			return prepared, err
		}
		source, err := workers.contributionPublicationMemorySource(read, job.RepoID, record.Capture, publication)
		if err != nil {
			return prepared, err
		}
		memory := (domain.GitContributionMemory{RepoID: job.RepoID, ContributionID: record.ID, PublicationID: publication.ID, MemorySource: source, MemoryHash: publication.MemoryHash, MemoryPinned: true}).WithID()
		if memory.Validate() != nil {
			return prepared, domain.ErrIntegrity
		}
		return preparedGitContributionSupplement{job: job, proof: proof, admission: admission, head: head, publication: publication, memory: memory, revision: snapshot.revision, snapshot: snapshot}, nil
	})
}

func (workers *GitIntegrationWorkers) applyGitContributionSupplement(ctx context.Context, prepared preparedGitContributionSupplement, authorization gitAdmissionAuthorization) error {
	job := prepared.job
	ledger, supported := workers.work.meta.(outbound.GitContributionStore)
	queue, queued := workers.work.meta.(outbound.GitContributionSupplementStore)
	_, atomic := workers.work.meta.(outbound.RepositoryTransactions)
	if !supported || !queued || !atomic || authorization.fence == nil || job.Validate() != nil || job.State != "processing" || prepared.memory.Validate() != nil {
		return domain.ErrValidation
	}
	if prepared.proof.Validate() != nil || prepared.admission.Validate() != nil || prepared.head.Validate() != nil || prepared.proof.ID != job.ProofID || prepared.proof.Contribution.ID != job.ContributionID || prepared.proof.Contribution.RepoID != job.RepoID || prepared.proof.Contribution.GitOrigin != job.GitOrigin || prepared.admission.ID != prepared.proof.AdmissionID || prepared.head.ID != prepared.proof.ContextHeadID || prepared.publication.ID != job.PublicationID {
		return domain.ErrIntegrity
	}
	if authorization.repo != job.RepoID || authorization.provider.RepositoryID != prepared.admission.Destination.ProviderRepositoryID || authorization.provider.InstallationID != prepared.admission.Destination.InstallationID {
		return domain.ErrConflict
	}
	return repositoryWriteErrorWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), workers.work, job.RepoID, func(write context.Context) error {
		if err := authorization.fence(write); err != nil {
			return err
		}
		current, err := workers.workerRepository(write, job.RepoID)
		if err != nil {
			return err
		}
		if current.GitRemoteURL != job.GitOrigin {
			return domain.ErrConflict
		}
		proof, err := queue.GetGitContributionSupplementProof(write, job.RepoID, job.ProofID)
		if err != nil {
			return err
		}
		if proof != prepared.proof {
			return domain.ErrIntegrity
		}
		snapshot, err := workers.reuseGitAdmissionSnapshot(write, job.RepoID, prepared.snapshot)
		if err != nil {
			return err
		}
		if snapshot.revision.Graph != prepared.revision.Graph || snapshot.revision.Evidence != prepared.revision.Evidence {
			return domain.ErrConflict
		}
		if _, err := snapshot.admittedPublication(prepared.admission, prepared.head); err != nil {
			return err
		}
		publication, err := gitSupplementPublication(snapshot.index, proof.Contribution, job.PublicationID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(publication, prepared.publication) {
			return domain.ErrConflict
		}
		source, err := workers.contributionPublicationMemorySource(write, job.RepoID, proof.Contribution.Capture, publication)
		if err != nil {
			return err
		}
		want := (domain.GitContributionMemory{RepoID: job.RepoID, ContributionID: proof.Contribution.ID, PublicationID: publication.ID, MemorySource: source, MemoryHash: publication.MemoryHash, MemoryPinned: true}).WithID()
		if want != prepared.memory {
			return domain.ErrIntegrity
		}
		pin, err := queue.FindGitContributionSupplementMemory(write, job.RepoID, want.ContributionID, want.PublicationID)
		if err == nil && pin != want {
			return domain.ErrIntegrity
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if errors.Is(err, domain.ErrNotFound) {
			if err := ledger.PutGitContributionMemory(write, want); err != nil {
				return err
			}
			if err := workers.work.revisionWrite(evidenceWriteContext(write), job.RepoID); err != nil {
				return err
			}
		}
		job.State, job.Reason, job.LeaseUntil = "completed", "", time.Time{}
		return queue.FinishGitContributionSupplement(write, job)
	})
}
