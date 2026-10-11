package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) gitContributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]verifiedGitContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := evidence.bindContributionHistory(repo, history); err != nil {
		return nil, err
	}
	if evidence != nil && evidence.gitContributionsLoaded {
		return evidence.gitContributions, nil
	}
	rows, err := s.queryGitContributions(ctx, repo, history)
	if err == nil && evidence != nil {
		evidence.gitContributions, evidence.gitContributionsLoaded = rows, true
	}
	return rows, err
}

func (s *Service) queryGitContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]verifiedGitContribution, error) {
	ledger, ok := s.meta.(outbound.GitContributionStore)
	if !ok {
		return nil, nil
	}
	records, err := ledger.ListGitContributions(ctx, repo)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	if len(records) > maxProjectionSnapshots {
		return nil, domain.ErrMemoryProjectionLimit
	}
	byID := map[string]int{}
	out := make([]verifiedGitContribution, 0, len(records))
	for _, record := range records {
		if record.Validate() != nil || record.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		byID[record.ID] = len(out)
		out = append(out, verifiedGitContribution{record: record, publications: map[string]domain.HistoryEvent{}})
	}
	headStore, headsSupported := s.meta.(outbound.GitContextHeadStore)
	admissionStore, admissionsSupported := s.meta.(outbound.GitContributionAdmissionStore)
	receiptStore, receiptsSupported := s.meta.(outbound.GitHeadReceiptStore)
	if !headsSupported || !admissionsSupported || !receiptsSupported {
		return out, nil
	}
	proofs, err := headStore.ListGitContributionProofs(ctx, repo)
	if err != nil {
		return nil, err
	}
	if len(proofs) > maxProjectionSnapshots {
		return nil, domain.ErrMemoryProjectionLimit
	}
	snapshot, err := s.readGitAdmissionHistory(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	for _, proof := range proofs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, exists := byID[proof.Contribution.ID]
		if !exists || proof.Contribution != out[index].record {
			return nil, domain.ErrIntegrity
		}
		admission, err := admissionStore.GetGitContributionAdmission(ctx, repo, proof.AdmissionID)
		if err != nil {
			return nil, err
		}
		head, err := headStore.GetGitContextHead(ctx, repo, proof.ContextHeadID)
		if err != nil {
			return nil, err
		}
		receipt, err := receiptStore.GetGitHeadReceipt(ctx, repo, head.Destination.GitOrigin, head.Destination.ReadVersion)
		if err != nil {
			return nil, err
		}
		if !proof.MatchesInputs(admission, head, receipt) {
			return nil, domain.ErrIntegrity
		}
		out[index].sourceBranch, out[index].destinationBranch = admission.SourceBranch, head.Destination.DestinationBranch
		if proof.Contribution.GitOrigin != snapshot.repo.GitRemoteURL {
			continue
		}
		publication, err := snapshot.admittedPublication(admission, head)
		if gitAdmissionDeferred(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		baseline, err := receiptStore.GetGitHeadReceipt(ctx, repo, admission.Destination.GitOrigin, admission.Destination.ReadVersion)
		if err != nil {
			return nil, err
		}
		if !admission.MatchesBaseline(baseline) {
			return nil, domain.ErrIntegrity
		}
		out[index].valid = true
		out[index].publications[publication.ID] = publication
	}
	type publicationKey struct {
		branch  string
		capture domain.ContentHash
		commit  string
	}
	eligible := map[publicationKey][]int{}
	for index, contribution := range out {
		if contribution.valid {
			key := publicationKey{contribution.record.SourceBranchID, contribution.record.Capture, contribution.record.OriginalHead}
			eligible[key] = append(eligible[key], index)
		}
	}
	for _, event := range snapshot.history {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Kind != "publish" || !event.MemoryPinned {
			continue
		}
		for _, index := range eligible[publicationKey{event.BranchID, event.Target, event.GitAfter}] {
			publication, err := gitSupplementPublication(snapshot.index, out[index].record, event.ID)
			if gitAdmissionDeferred(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[index].publications[publication.ID] = publication
		}
	}
	return out, nil
}
