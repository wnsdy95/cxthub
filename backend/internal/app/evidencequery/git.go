package evidencequery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) VerifiedGit(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]VerifiedGitContribution, error) {
	if queries.gitLedger == nil {
		return nil, nil
	}
	records, err := queries.gitLedger.ListGitContributions(ctx, repo)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	if len(records) > contextprojection.MaxProjectionSnapshots {
		return nil, domain.ErrMemoryProjectionLimit
	}
	byID := map[string]int{}
	out := make([]VerifiedGitContribution, 0, len(records))
	for _, record := range records {
		if record.Validate() != nil || record.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		byID[record.ID] = len(out)
		out = append(out, VerifiedGitContribution{Record: record, Publications: map[string]domain.HistoryEvent{}})
	}
	if queries.gitHeads == nil || queries.gitAdmission == nil || queries.gitReceipts == nil {
		return out, nil
	}
	proofs, err := queries.gitHeads.ListGitContributionProofs(ctx, repo)
	if err != nil {
		return nil, err
	}
	if len(proofs) > contextprojection.MaxProjectionSnapshots {
		return nil, domain.ErrMemoryProjectionLimit
	}
	metadata, err := queries.metadata.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	snapshot, err := domain.NewGitPublicationEvidence(ctx, repo, metadata, history)
	if err != nil {
		return nil, err
	}
	for _, proof := range proofs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, exists := byID[proof.Contribution.ID]
		if !exists || proof.Contribution != out[index].Record {
			return nil, domain.ErrIntegrity
		}
		admission, err := queries.gitAdmission.GetGitContributionAdmission(ctx, repo, proof.AdmissionID)
		if err != nil {
			return nil, err
		}
		head, err := queries.gitHeads.GetGitContextHead(ctx, repo, proof.ContextHeadID)
		if err != nil {
			return nil, err
		}
		receipt, err := queries.gitReceipts.GetGitHeadReceipt(ctx, repo, head.Destination.GitOrigin, head.Destination.ReadVersion)
		if err != nil {
			return nil, err
		}
		if !proof.MatchesInputs(admission, head, receipt) {
			return nil, domain.ErrIntegrity
		}
		out[index].SourceBranch, out[index].DestinationBranch = admission.SourceBranch, head.Destination.DestinationBranch
		if proof.Contribution.GitOrigin != metadata.GitRemoteURL {
			continue
		}
		publication, err := domain.GitAdmittedPublication(metadata, snapshot.Index, snapshot.History, admission, head)
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		baseline, err := queries.gitReceipts.GetGitHeadReceipt(ctx, repo, admission.Destination.GitOrigin, admission.Destination.ReadVersion)
		if err != nil {
			return nil, err
		}
		if !admission.MatchesBaseline(baseline) {
			return nil, domain.ErrIntegrity
		}
		out[index].Valid = true
		out[index].Publications[publication.ID] = publication
	}
	type publicationKey struct {
		branch  string
		capture domain.ContentHash
		commit  string
	}
	eligible := map[publicationKey][]int{}
	for index, contribution := range out {
		if contribution.Valid {
			key := publicationKey{contribution.Record.SourceBranchID, contribution.Record.Capture, contribution.Record.OriginalHead}
			eligible[key] = append(eligible[key], index)
		}
	}
	for _, event := range snapshot.History {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Kind != "publish" || !event.MemoryPinned {
			continue
		}
		for _, index := range eligible[publicationKey{event.BranchID, event.Target, event.GitAfter}] {
			publication, err := domain.GitSupplementPublication(snapshot.Index, out[index].Record, event.ID)
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[index].Publications[publication.ID] = publication
		}
	}
	return out, nil
}
