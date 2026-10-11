package app

import (
	"context"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextContributionProjection struct {
	record            contributionMemoryRecord
	valid             bool
	publications      map[string]domain.HistoryEvent
	sourceBranch      string
	destinationBranch string
	prNumber          int
	mergedAt          time.Time
}

func (s *Service) contextContributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]contextContributionProjection, error) {
	prs, err := s.contributionsForEvidence(ctx, repo, history, evidence)
	if err != nil {
		return nil, err
	}
	git, err := s.gitContributionsForEvidence(ctx, repo, history, evidence)
	if err != nil {
		return nil, err
	}
	result := make([]contextContributionProjection, 0, len(prs)+len(git))
	for _, verified := range prs {
		record := verified.record
		result = append(result, contextContributionProjection{record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID, SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.PR.MergeSHA}, valid: verified.valid, publications: verified.publications, sourceBranch: record.PR.HeadBranch, destinationBranch: record.PR.BaseBranch, prNumber: record.PR.Number, mergedAt: verified.mergedAt})
	}
	for _, verified := range git {
		record := verified.record
		result = append(result, contextContributionProjection{record: contributionMemoryRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID, SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.IntegrationSHA}, valid: verified.valid, publications: verified.publications, sourceBranch: verified.sourceBranch, destinationBranch: verified.destinationBranch})
	}
	return result, nil
}

func (s *Service) contributionsForEvidence(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *codeEvidence) ([]verifiedPRContribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := evidence.bindContributionHistory(repo, history); err != nil {
		return nil, err
	}
	if evidence != nil && evidence.contributionsLoaded {
		return evidence.contributions, nil
	}
	rows, err := s.verifiedPRContributions(ctx, repo, history)
	if err == nil && evidence != nil {
		evidence.contributions, evidence.contributionsLoaded = rows, true
	}
	return rows, err
}

func contributionMergeKey(scope string, source domain.ContentHash, merge string) string {
	return scope + "\x00" + string(source) + "\x00" + merge
}

func appendContributionMerges(inclusion *domain.BranchContext, scopes map[string]bool, records []contextContributionProjection) {
	existing := map[string]int{}
	for index, merge := range inclusion.Merges {
		existing[contributionMergeKey(merge.DestinationBranchID, merge.Source, merge.MergeSHA)] = index
	}
	groups := map[string]contextContributionProjection{}
	for _, verified := range records {
		record := verified.record
		key := contributionMergeKey(record.DestinationBranchID, record.Capture, record.IntegrationSHA)
		old, exists := groups[key]
		if !exists || (!old.valid && verified.valid) {
			groups[key] = verified
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		verified := groups[key]
		record := verified.record
		if !scopes[record.DestinationBranchID] {
			continue
		}
		if index, exists := existing[key]; exists {
			inclusion.Merges[index].DeliveryAcknowledged = verified.valid
			if !verified.valid {
				inclusion.Merges[index].State, inclusion.Merges[index].Reason = "review", "participation_evidence_conflict"
			}
			continue
		}
		reason := "git_evidence_pending"
		if !verified.valid {
			reason = "participation_evidence_conflict"
		}
		inclusion.Merges = append(inclusion.Merges, domain.BranchContextMerge{DeliveryAcknowledged: verified.valid, DestinationBranchID: record.DestinationBranchID, EventID: record.ID, Source: record.Capture, MergeSHA: record.IntegrationSHA, PRNumber: verified.prNumber, State: "review", Reason: reason, Order: -1})
		existing[key] = len(inclusion.Merges) - 1
	}
}

func appendContributionOperations(graph *domain.GraphState, records []contextContributionProjection) {
	existing := map[string]bool{}
	for _, operation := range graph.Operations.Merges {
		existing[operation.EventID] = true
	}
	byID := map[string]contextContributionProjection{}
	for _, record := range records {
		byID[record.record.ID] = record
	}
	branches := make([]string, 0, len(graph.BranchContexts))
	for branch := range graph.BranchContexts {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	for _, branch := range branches {
		inclusion := graph.BranchContexts[branch]
		for _, merge := range inclusion.Merges {
			if existing[merge.EventID] {
				continue
			}
			verified, found := byID[merge.EventID]
			if !found {
				continue
			}
			record := verified.record
			destination := verified.destinationBranch
			if head, exists := graph.BranchHeads[record.DestinationBranchID]; exists {
				destination = head.Branch
			}
			graph.Operations.Merges = append(graph.Operations.Merges, domain.GraphMerge{ID: "graph:merge:" + record.ID, EventID: record.ID, Source: record.Capture, Branch: destination, Scope: record.DestinationBranchID, From: verified.sourceBranch, CreatedAt: verified.mergedAt, PRNumber: verified.prNumber, HistoricalOnly: true, RepresentedGrafts: []domain.ContentHash{}, RedirectChildren: []domain.ContentHash{}})
			existing[record.ID] = true
		}
	}
}
