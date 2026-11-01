package contextprojection

import (
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPolicyContributions struct {
	rows      []Contribution
	positions []int
}

func (records *branchPolicyContributions) Len() int { return len(records.rows) }

func (records *branchPolicyContributions) At(position int) Contribution {
	records.positions = append(records.positions, position)
	return records.rows[position]
}

func TestContributionPolicyMergeKeyUsesSharedIdentity(test *testing.T) {
	if got := ContributionMergeKey("scope", "capture", "merge"); got != "scope\x00capture\x00merge" {
		test.Fatal("contribution identity changed", got)
	}
	keys := map[string]bool{}
	for _, record := range []Contribution{
		{DestinationBranchID: "scope", Capture: "capture", IntegrationSHA: "merge"},
		{DestinationBranchID: "other", Capture: "capture", IntegrationSHA: "merge"},
		{DestinationBranchID: "scope", Capture: "other", IntegrationSHA: "merge"},
		{DestinationBranchID: "scope", Capture: "capture", IntegrationSHA: "other"},
	} {
		key := ContributionMergeKey(record.DestinationBranchID, record.Capture, record.IntegrationSHA)
		if keys[key] {
			test.Fatal("scope/capture/integration identity collapsed", record)
		}
		keys[key] = true
	}
}

func TestContributionPolicyMergesPreferFirstValidDuplicate(test *testing.T) {
	invalid := Contribution{ID: "invalid", DestinationBranchID: "main-id", Capture: "source", IntegrationSHA: "merge", PRNumber: 1}
	first, second := invalid, invalid
	first.ID, first.Valid, first.PRNumber = "first-valid", true, 2
	second.ID, second.Valid, second.PRNumber = "second-valid", true, 3
	for _, rows := range [][]Contribution{{invalid, first, second}, {first, invalid, second}, {first, second, invalid}} {
		inclusion := domain.BranchContext{Merges: []domain.BranchContextMerge{}}
		records := &branchPolicyContributions{rows: rows}
		AppendContributionMerges(&inclusion, map[string]bool{"main-id": true}, records)
		want := []domain.BranchContextMerge{{DeliveryAcknowledged: true, DestinationBranchID: "main-id", EventID: "first-valid", Source: "source", MergeSHA: "merge", PRNumber: 2, State: "review", Reason: "git_evidence_pending", Order: -1}}
		if !reflect.DeepEqual(inclusion.Merges, want) || !reflect.DeepEqual(records.positions, []int{0, 1, 2}) {
			test.Fatalf("duplicate preference/accessor changed: %+v, %+v", inclusion.Merges, records.positions)
		}
	}
	invalid.ID = "first-invalid"
	second = invalid
	second.ID = "second-invalid"
	inclusion := domain.BranchContext{}
	AppendContributionMerges(&inclusion, map[string]bool{"main-id": true}, &branchPolicyContributions{rows: []Contribution{invalid, second}})
	if len(inclusion.Merges) != 1 || inclusion.Merges[0].EventID != invalid.ID || inclusion.Merges[0].DeliveryAcknowledged || inclusion.Merges[0].Reason != "participation_evidence_conflict" {
		test.Fatalf("invalid duplicate diagnostic changed: %+v", inclusion)
	}
}

func TestContributionPolicyCoalescesWithoutReplacingCompletionIdentity(test *testing.T) {
	for _, valid := range []bool{false, true} {
		original := domain.BranchContextMerge{DeliveryAcknowledged: true, DestinationBranchID: "main-id", EventID: "completed-receipt", Source: "capture", Before: "before", MergeSHA: "integration", PRNumber: 8, State: "included", Reason: "verified_git_order", Order: 7}
		inclusion := domain.BranchContext{Merges: []domain.BranchContextMerge{original}}
		AppendContributionMerges(&inclusion, map[string]bool{"main-id": true}, &branchPolicyContributions{rows: []Contribution{{ID: "ledger-identity", DestinationBranchID: "main-id", Capture: "capture", IntegrationSHA: "integration", PRNumber: 99, Valid: valid}}})
		want := original
		want.DeliveryAcknowledged = valid
		if !valid {
			want.State, want.Reason = "review", "participation_evidence_conflict"
		}
		if !reflect.DeepEqual(inclusion.Merges, []domain.BranchContextMerge{want}) {
			test.Fatalf("completion identity/baseline/order replaced: %+v", inclusion.Merges)
		}
	}
}

func TestContributionPolicyMergesFilterScopesAndSortIdentityKeys(test *testing.T) {
	inclusion := domain.BranchContext{Merges: []domain.BranchContextMerge{{EventID: "existing", DestinationBranchID: "outside", Source: "outside", MergeSHA: "outside", DeliveryAcknowledged: true}}}
	records := &branchPolicyContributions{rows: []Contribution{
		{ID: "last", DestinationBranchID: "scope-b", Capture: "capture", IntegrationSHA: "merge", Valid: true},
		{ID: "outside", DestinationBranchID: "outside", Capture: "outside", IntegrationSHA: "outside", Valid: false},
		{ID: "middle", DestinationBranchID: "scope-a", Capture: "capture", IntegrationSHA: "second", Valid: false},
		{ID: "first", DestinationBranchID: "scope-a", Capture: "capture", IntegrationSHA: "first", Valid: true},
	}}
	AppendContributionMerges(&inclusion, map[string]bool{"scope-a": true, "scope-b": true}, records)
	var identifiers []string
	for _, merge := range inclusion.Merges {
		identifiers = append(identifiers, merge.EventID)
	}
	if !reflect.DeepEqual(identifiers, []string{"existing", "first", "middle", "last"}) || !inclusion.Merges[0].DeliveryAcknowledged || inclusion.Merges[2].Reason != "participation_evidence_conflict" {
		test.Fatalf("scope isolation/key ordering changed: %+v", inclusion.Merges)
	}
}

func TestContributionPolicyOperationsDeduplicateAndKeepInvalidDiagnostics(test *testing.T) {
	original := domain.GraphMerge{ID: "existing-operation", EventID: "existing", Branch: "untouched", HistoricalOnly: false}
	graph := domain.GraphState{
		Operations: domain.GraphOperations{Merges: []domain.GraphMerge{original}},
		BranchContexts: map[string]domain.BranchContext{
			"z-child": {Merges: []domain.BranchContextMerge{{EventID: "duplicate"}, {EventID: "invalid"}}},
			"a-main":  {Merges: []domain.BranchContextMerge{{EventID: "duplicate", State: "review"}, {EventID: "invalid", State: "review", Reason: "participation_evidence_conflict"}, {EventID: "existing"}, {EventID: "missing"}}},
		},
		BranchHeads: map[string]domain.GraphBranchHead{"main-id": {Branch: "renamed-main"}},
	}
	records := &branchPolicyContributions{rows: []Contribution{
		{ID: "duplicate", DestinationBranchID: "old-id", Capture: "old-capture", Valid: true, SourceBranch: "old-source", DestinationBranch: "old-destination", PRNumber: 1},
		{ID: "invalid", DestinationBranchID: "archived-id", Capture: "invalid-capture", Valid: false, SourceBranch: "invalid-source", DestinationBranch: "archived-destination", PRNumber: 20, MergedAt: time.Unix(200, 0)},
		{ID: "unreferenced", Valid: true},
		{ID: "existing", Capture: "must-not-replace"},
		{ID: "duplicate", DestinationBranchID: "main-id", Capture: "last-capture", Valid: false, SourceBranch: "last-source", DestinationBranch: "stale-name", PRNumber: 30, MergedAt: time.Unix(300, 0)},
	}}
	AppendContributionOperations(&graph, records)
	want := []domain.GraphMerge{
		original,
		{ID: "graph:merge:duplicate", EventID: "duplicate", Source: "last-capture", Branch: "renamed-main", Scope: "main-id", From: "last-source", CreatedAt: time.Unix(300, 0), PRNumber: 30, HistoricalOnly: true, RepresentedGrafts: []domain.ContentHash{}, RedirectChildren: []domain.ContentHash{}},
		{ID: "graph:merge:invalid", EventID: "invalid", Source: "invalid-capture", Branch: "archived-destination", Scope: "archived-id", From: "invalid-source", CreatedAt: time.Unix(200, 0), PRNumber: 20, HistoricalOnly: true, RepresentedGrafts: []domain.ContentHash{}, RedirectChildren: []domain.ContentHash{}},
	}
	if !reflect.DeepEqual(graph.Operations.Merges, want) || !reflect.DeepEqual(records.positions, []int{0, 1, 2, 3, 4}) {
		test.Fatalf("operation mapping/last-ID/dedup changed: %+v, reads=%+v", graph.Operations.Merges, records.positions)
	}
}

func TestContributionPolicyOperationsFollowSortedBranches(test *testing.T) {
	graph := domain.GraphState{BranchContexts: map[string]domain.BranchContext{
		"z-branch": {Merges: []domain.BranchContextMerge{{EventID: "first-record"}}},
		"a-branch": {Merges: []domain.BranchContextMerge{{EventID: "second-record"}}},
	}}
	AppendContributionOperations(&graph, &branchPolicyContributions{rows: []Contribution{{ID: "first-record"}, {ID: "second-record"}}})
	if len(graph.Operations.Merges) != 2 || graph.Operations.Merges[0].EventID != "second-record" || graph.Operations.Merges[1].EventID != "first-record" {
		test.Fatalf("record/map order replaced branch ordering: %+v", graph.Operations.Merges)
	}
}

func TestContributionPolicyOperationsDoNotInventLedgerAliasForCompletion(test *testing.T) {
	graph := domain.GraphState{BranchContexts: map[string]domain.BranchContext{"main": {Merges: []domain.BranchContextMerge{{EventID: "completion", DestinationBranchID: "main-id", Source: "capture", MergeSHA: "integration"}}}}}
	AppendContributionOperations(&graph, &branchPolicyContributions{rows: []Contribution{{ID: "ledger", DestinationBranchID: "main-id", Capture: "capture", IntegrationSHA: "integration", Valid: true}}})
	if len(graph.Operations.Merges) != 0 {
		test.Fatalf("operation matched by key instead of event ID: %+v", graph.Operations.Merges)
	}
}
