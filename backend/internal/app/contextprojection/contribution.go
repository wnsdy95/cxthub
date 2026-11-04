package contextprojection

import (
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func ContributionMergeKey(scope string, source domain.ContentHash, merge string) string {
	return scope + "\x00" + string(source) + "\x00" + merge
}

func AppendContributionMerges(inclusion *domain.BranchContext, scopes map[string]bool, records Contributions) {
	existing := map[string]int{}
	for index, merge := range inclusion.Merges {
		existing[ContributionMergeKey(merge.DestinationBranchID, merge.Source, merge.MergeSHA)] = index
	}
	groups := map[string]Contribution{}
	for position := 0; position < records.Len(); position++ {
		verified := records.At(position)
		key := ContributionMergeKey(verified.DestinationBranchID, verified.Capture, verified.IntegrationSHA)
		old, exists := groups[key]
		if !exists || (!old.Valid && verified.Valid) {
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
		if !scopes[verified.DestinationBranchID] {
			continue
		}
		if index, exists := existing[key]; exists {
			inclusion.Merges[index].DeliveryAcknowledged = verified.Valid
			if !verified.Valid {
				inclusion.Merges[index].State, inclusion.Merges[index].Reason = "review", "participation_evidence_conflict"
			}
			continue
		}
		reason := "git_evidence_pending"
		if !verified.Valid {
			reason = "participation_evidence_conflict"
		}
		inclusion.Merges = append(inclusion.Merges, domain.BranchContextMerge{DeliveryAcknowledged: verified.Valid, DestinationBranchID: verified.DestinationBranchID, EventID: verified.ID, Source: verified.Capture, MergeSHA: verified.IntegrationSHA, PRNumber: verified.PRNumber, State: "review", Reason: reason, Order: -1})
		existing[key] = len(inclusion.Merges) - 1
	}
}

func AppendContributionOperations(graph *domain.GraphState, records Contributions) {
	existing := map[string]bool{}
	for _, operation := range graph.Operations.Merges {
		existing[operation.EventID] = true
	}
	byID := map[string]Contribution{}
	for position := 0; position < records.Len(); position++ {
		record := records.At(position)
		byID[record.ID] = record
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
			destination := verified.DestinationBranch
			if head, exists := graph.BranchHeads[verified.DestinationBranchID]; exists {
				destination = head.Branch
			}
			graph.Operations.Merges = append(graph.Operations.Merges, domain.GraphMerge{ID: "graph:merge:" + verified.ID, EventID: verified.ID, Source: verified.Capture, Branch: destination, Scope: verified.DestinationBranchID, From: verified.SourceBranch, CreatedAt: verified.MergedAt, PRNumber: verified.PRNumber, HistoricalOnly: true, RepresentedGrafts: []domain.ContentHash{}, RedirectChildren: []domain.ContentHash{}})
			existing[verified.ID] = true
		}
	}
}
