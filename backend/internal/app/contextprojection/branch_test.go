package contextprojection

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPolicyEvidence struct {
	commits       map[string]domain.GitCommitTree
	treeCalls     []string
	treeError     error
	relations     map[[2]string]string
	relationCalls [][2]string
	relationError error
	exhaustCalls  int
	exhaustError  error
	reads         int
	strict        bool
}

func (evidence *branchPolicyEvidence) Origin() string { return "https://git.test/policy" }

func (evidence *branchPolicyEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	evidence.treeCalls = append(evidence.treeCalls, identifier)
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	if repo != branchPolicyHash("repo") || origin != evidence.Origin() {
		return domain.GitCommitTree{}, domain.ErrIntegrity
	}
	if evidence.reads >= MaxCodeEvidenceReads {
		if evidence.strict {
			return domain.GitCommitTree{}, domain.ErrMemoryProjectionLimit
		}
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	evidence.reads++
	if evidence.treeError != nil {
		return domain.GitCommitTree{}, evidence.treeError
	}
	commit, exists := evidence.commits[identifier]
	if !exists {
		return domain.GitCommitTree{}, domain.ErrNotFound
	}
	return commit, nil
}

func (evidence *branchPolicyEvidence) Relation(ctx context.Context, source, selected string) (string, error) {
	key := [2]string{source, selected}
	evidence.relationCalls = append(evidence.relationCalls, key)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if evidence.relationError != nil {
		return "", evidence.relationError
	}
	if relation, exists := evidence.relations[key]; exists {
		return relation, nil
	}
	return "unknown", nil
}

func (evidence *branchPolicyEvidence) ExhaustTraversal() error {
	evidence.exhaustCalls++
	return evidence.exhaustError
}

type branchPolicyCancellationContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *branchPolicyCancellationContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func branchPolicyHash(value string) domain.ContentHash {
	return domain.HashContent([]byte(value))
}

func branchPolicyOID(value int) string { return fmt.Sprintf("%040x", value) }

type branchPolicyFixture struct {
	ref       domain.Ref
	snapshots []domain.Snapshot
	history   []domain.HistoryEvent
	first     domain.ContentHash
	second    domain.ContentHash
	before    domain.ContentHash
	common    domain.ContentHash
	evidence  *branchPolicyEvidence
}

func newBranchPolicyFixture() branchPolicyFixture {
	repo := branchPolicyHash("repo")
	fixture := branchPolicyFixture{
		ref:   domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: branchPolicyHash("continued")},
		first: branchPolicyHash("first"), second: branchPolicyHash("second"), before: branchPolicyHash("before"), common: branchPolicyHash("common"),
		evidence: &branchPolicyEvidence{commits: map[string]domain.GitCommitTree{}, relations: map[[2]string]string{}},
	}
	fixture.snapshots = []domain.Snapshot{
		{RepoID: repo, ID: fixture.ref.Target, CreatedAt: time.Unix(900, 0)},
		{RepoID: repo, ID: fixture.first, Parents: []domain.ContentHash{fixture.common}, CreatedAt: time.Unix(500, 0)},
		{RepoID: repo, ID: fixture.second, Parents: []domain.ContentHash{fixture.common}, CreatedAt: time.Unix(100, 0)},
		{RepoID: repo, ID: fixture.before, Parents: []domain.ContentHash{fixture.common}},
		{RepoID: repo, ID: fixture.common},
	}
	for number := 1; number <= 4; number++ {
		commit := domain.GitCommitTree{Commit: branchPolicyOID(number), Parents: []string{}}
		if number > 1 {
			commit.Parents = []string{branchPolicyOID(number - 1)}
		}
		fixture.evidence.commits[commit.Commit] = commit
	}
	for position, source := range []domain.ContentHash{fixture.first, fixture.second} {
		receipt := domain.HistoryEvent{ID: fmt.Sprintf("%032x", position+100), RepoID: string(repo), Branch: "main", BranchID: fixture.ref.BranchID, Kind: "pr-merge", Source: source, Target: source, SourceBranchID: fmt.Sprintf("feature-%d", position), PR: &domain.PullRequestMerge{Number: position + 10, BaseBranch: "main", HeadBranch: fmt.Sprintf("feature-%d", position), HeadSHA: branchPolicyOID(position + 8), MergeSHA: branchPolicyOID(position + 2)}, CreatedAt: time.Unix(int64(300-position*100), 0)}
		completion := receipt
		key := sha256.Sum256([]byte(receipt.ID + ":completed"))
		completion.ID, completion.PRCompleted = fmt.Sprintf("%x", key[:16]), true
		completion.SharedTarget, completion.Target = fixture.before, fixture.ref.Target
		fixture.history = append(fixture.history, receipt, completion)
	}
	return fixture
}

func branchPolicyInputs(test *testing.T, fixture branchPolicyFixture) *Inputs {
	test.Helper()
	inputs, err := NewInputs(context.Background(), fixture.ref.RepoID, fixture.snapshots, fixture.history)
	if err != nil {
		test.Fatal(err)
	}
	return inputs
}

func TestBranchPolicyTwoPRsUseGitOrderNotReceiptArrival(test *testing.T) {
	for _, reverse := range []bool{false, true} {
		test.Run(fmt.Sprintf("reverse-arrival=%t", reverse), func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			if reverse {
				slices.Reverse(fixture.history)
			}
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), branchPolicyInputs(test, fixture), fixture.evidence)
			if err != nil || plan == nil {
				test.Fatalf("prepare: %v, %v", plan, err)
			}
			prepared := plan.Result()
			if len(prepared.Merges) != 2 || len(fixture.evidence.relationCalls) != 0 || !reflect.DeepEqual(prepared.Roots, []domain.ContentHash{fixture.ref.Target}) || len(prepared.SnapshotIDs) != 0 {
				test.Fatalf("prepare crossed contribution boundary: %+v", prepared)
			}
			for _, merge := range prepared.Merges {
				if merge.State != "review" || merge.Reason != "git_evidence_pending" || merge.Order != -1 || !merge.DeliveryAcknowledged || merge.Before != fixture.before {
					test.Fatalf("receipt preparation changed: %+v", merge)
				}
			}
			records := &branchPolicyContributions{rows: []Contribution{
				{ID: "newer-contribution", DestinationBranchID: fixture.ref.BranchID, Capture: fixture.second, IntegrationSHA: branchPolicyOID(3), Valid: true},
				{ID: "older-contribution", DestinationBranchID: fixture.ref.BranchID, Capture: fixture.first, IntegrationSHA: branchPolicyOID(2), Valid: true},
			}}
			got, err := plan.Finish(context.Background(), records, fixture.evidence)
			if err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(got.Roots, []domain.ContentHash{fixture.before, fixture.first, fixture.second, fixture.ref.Target}) || !reflect.DeepEqual(got.SnapshotIDs, []domain.ContentHash{fixture.ref.Target, fixture.second, fixture.first, fixture.before, fixture.common}) {
				test.Fatalf("Git ordering lost: %+v", got)
			}
			if len(got.Merges) != 2 || got.Merges[0].Source != fixture.first || got.Merges[0].Order != 2 || got.Merges[1].Source != fixture.second || got.Merges[1].Order != 1 {
				test.Fatalf("arrival or timestamps replaced Git order: %+v", got.Merges)
			}
			for _, merge := range got.Merges {
				if merge.State != "included" || merge.Reason != "verified_git_order" || !merge.DeliveryAcknowledged || merge.EventID == "newer-contribution" || merge.EventID == "older-contribution" {
					test.Fatalf("completion coalescing changed: %+v", merge)
				}
			}
			if !reflect.DeepEqual(plan.Result(), got) || !reflect.DeepEqual(records.positions, []int{0, 1}) {
				test.Fatalf("result or accessor mapping changed: %+v, %+v", plan.Result(), records.positions)
			}
		})
	}
}

func TestBranchPolicyInheritedBirthIgnoresNamesAndGrafts(test *testing.T) {
	fixture := newBranchPolicyFixture()
	main := fixture.ref
	birthSource, unrelated := branchPolicyHash("birth-source"), branchPolicyHash("unrelated")
	fixture.snapshots = append(fixture.snapshots,
		domain.Snapshot{RepoID: main.RepoID, ID: birthSource, Parents: []domain.ContentHash{main.Target}, GraftParents: []domain.ContentHash{unrelated}},
		domain.Snapshot{RepoID: main.RepoID, ID: unrelated},
	)
	fixture.ref.BranchID, fixture.ref.Target = "child-id", birthSource
	fixture.history = append(fixture.history,
		domain.HistoryEvent{ID: "publication", RepoID: string(main.RepoID), Kind: "publish", Branch: "renamed-main", BranchID: main.BranchID, Source: main.Target, Target: main.Target, GitAfter: branchPolicyOID(4)},
		domain.HistoryEvent{ID: "name-parent", RepoID: string(main.RepoID), Kind: "publish", Branch: main.Name, BranchID: "unrelated-id", Source: unrelated, Target: unrelated, GitAfter: branchPolicyOID(4)},
		domain.HistoryEvent{ID: "birth", RepoID: string(main.RepoID), Kind: "birth", Branch: main.Name, BranchID: fixture.ref.BranchID, Source: birthSource, Target: birthSource, BindingParent: "name-parent"},
		domain.HistoryEvent{ID: "rename", RepoID: string(main.RepoID), Kind: "rename", Branch: main.Name, BranchID: fixture.ref.BranchID, NameParent: "name-parent"},
	)
	inputs := branchPolicyInputs(test, fixture)
	scopes, err := InheritedIntegrationScopes(context.Background(), fixture.ref.BranchID, inputs.snapshots, inputs.history)
	if err != nil || !reflect.DeepEqual(scopes, map[string]bool{"child-id": true, main.BranchID: true}) {
		test.Fatalf("inheritance followed names or overlays: %+v, %v", scopes, err)
	}
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(2), inputs, fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	fixture.evidence.relations[[2]string{branchPolicyOID(3), branchPolicyOID(2)}] = "not_ancestor"
	got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
	if err != nil || len(got.Merges) != 2 {
		test.Fatalf("inherited receipts missing: %+v, %v", got, err)
	}
	for _, merge := range got.Merges {
		if merge.DestinationBranchID != main.BranchID || !merge.DeliveryAcknowledged || (merge.Source == fixture.second && merge.State != "not_selected") || (merge.Source == fixture.first && merge.State != "included") {
			test.Fatalf("inherited destination or selected code changed: %+v", merge)
		}
	}
	fixture.ref.BranchID = "recreated-main"
	plan, err = PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), inputs, fixture.evidence)
	if err != nil || len(plan.Result().Merges) != 0 {
		test.Fatalf("reused branch name inherited receipts: %+v, %v", plan, err)
	}
}

func TestBranchPolicyLateOverlayCannotRestoreUnselectedPR(test *testing.T) {
	fixture := newBranchPolicyFixture()
	future, preserved := branchPolicyHash("future"), branchPolicyHash("preserved")
	fixture.snapshots[0].Parents = []domain.ContentHash{fixture.common}
	fixture.snapshots[0].GraftParents = []domain.ContentHash{fixture.second, preserved}
	fixture.snapshots[1].GraftParents = []domain.ContentHash{fixture.second}
	fixture.snapshots[3].GraftParents = []domain.ContentHash{future}
	fixture.snapshots = append(fixture.snapshots, domain.Snapshot{RepoID: fixture.ref.RepoID, ID: future}, domain.Snapshot{RepoID: fixture.ref.RepoID, ID: preserved})
	fixture.evidence.relations[[2]string{branchPolicyOID(3), branchPolicyOID(2)}] = "not_ancestor"
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(2), branchPolicyInputs(test, fixture), fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	for _, source := range []domain.ContentHash{fixture.first, fixture.before, fixture.common, fixture.ref.Target, preserved} {
		if !slices.Contains(got.SnapshotIDs, source) {
			test.Fatalf("selected closure lost %s: %+v", source, got)
		}
	}
	if slices.Contains(got.SnapshotIDs, fixture.second) || slices.Contains(got.SnapshotIDs, future) || got.Merges[1].Reason != "merge_not_in_selected_code" {
		test.Fatalf("late mutable overlay imported future context: %+v", got)
	}
}

func TestBranchPolicyInvalidContributionCannotBeReincludedByCodeOrOverlay(test *testing.T) {
	fixture := newBranchPolicyFixture()
	fixture.snapshots[0].GraftParents = []domain.ContentHash{fixture.first}
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), branchPolicyInputs(test, fixture), fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	records := &branchPolicyContributions{rows: []Contribution{{ID: "invalid-participation", DestinationBranchID: fixture.ref.BranchID, Capture: fixture.first, IntegrationSHA: branchPolicyOID(2), Valid: false}}}
	got, err := plan.Finish(context.Background(), records, fixture.evidence)
	if err != nil || len(got.Merges) != 2 || slices.Contains(got.SnapshotIDs, fixture.first) || !slices.Contains(got.SnapshotIDs, fixture.second) || !slices.Contains(got.SnapshotIDs, fixture.common) || len(fixture.evidence.relationCalls) != 0 {
		test.Fatalf("Git order/overlay overrode invalid participation: %+v, %v", got, err)
	}
	var conflict domain.BranchContextMerge
	for _, merge := range got.Merges {
		if merge.Source == fixture.first {
			conflict = merge
		}
	}
	if conflict.State != "review" || conflict.Reason != "participation_evidence_conflict" || conflict.DeliveryAcknowledged || conflict.Order != -1 || conflict.Before != fixture.before || conflict.EventID == "invalid-participation" {
		test.Fatalf("invalid participation replaced completion identity: %+v", conflict)
	}
}

func TestBranchPolicyPrepareErrorsAlwaysPreservePlan(test *testing.T) {
	for _, scenario := range []string{"cancellation first", "foreign index", "invalid code", "missing target", "Git read failure", "cyclic Git", "completion conflict", "receipt cancellation"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			inputs := branchPolicyInputs(test, fixture)
			var ctx context.Context = context.Background()
			code, wantReason, wantError, wantMerges, wantCalls := branchPolicyOID(4), "selected_code", error(domain.ErrIntegrity), 0, 0
			switch scenario {
			case "cancellation first":
				ctx = &branchPolicyCancellationContext{Context: ctx, cancelAt: 1}
				inputs.history.repo, code, wantError = branchPolicyHash("foreign"), "invalid", context.Canceled
			case "foreign index":
				inputs.history.repo, code = branchPolicyHash("foreign"), "invalid"
			case "invalid code":
				code, wantError = "invalid", domain.ErrValidation
				delete(inputs.snapshots, fixture.ref.Target)
			case "missing target":
				code, wantReason, wantError = "", "code_position_unavailable", domain.ErrNotFound
				delete(inputs.snapshots, fixture.ref.Target)
			case "Git read failure":
				fixture.evidence.treeError, wantCalls = domain.ErrIntegrity, 1
			case "cyclic Git":
				fixture.evidence.commits[code] = domain.GitCommitTree{Commit: code, Parents: []string{code}}
				wantCalls = 1
			case "completion conflict":
				inputs.history.rows[3].Source = fixture.ref.Target
				wantError, wantMerges, wantCalls = domain.ErrRefConflict, 1, 4
			case "receipt cancellation":
				ctx = &branchPolicyCancellationContext{Context: ctx, cancelAt: 7}
				wantError, wantMerges, wantCalls = context.Canceled, 1, 4
			}
			plan, err := PrepareBranchContext(ctx, fixture.ref, code, inputs, fixture.evidence)
			if plan == nil || !errors.Is(err, wantError) {
				test.Fatalf("prepare lost non-nil error plan: %+v, %v", plan, err)
			}
			got := plan.Result()
			if got.BranchID != fixture.ref.BranchID || got.SnapshotID != fixture.ref.Target || got.CodeCommit != code || got.Reason != wantReason || len(got.Merges) != wantMerges || len(got.SnapshotIDs) != 0 || got.SnapshotIDs == nil || got.Merges == nil || !reflect.DeepEqual(got.Roots, []domain.ContentHash{fixture.ref.Target}) || len(fixture.evidence.treeCalls) != wantCalls || len(fixture.evidence.relationCalls) != 0 {
				test.Fatalf("partial result or failure boundary changed: %+v, calls=%+v", got, fixture.evidence.treeCalls)
			}
		})
	}
}

func TestBranchPolicyFinishCancellationKeepsOriginalMutationBoundary(test *testing.T) {
	fixture := newBranchPolicyFixture()
	fixture.history = nil
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), branchPolicyInputs(test, fixture), fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	prepared := plan.Result()
	if prepared.Reason != "selected_code" || len(prepared.Merges) != 0 || len(fixture.evidence.relationCalls) != 0 {
		test.Fatalf("prepare performed post-I/O work: %+v", prepared)
	}
	records := &branchPolicyContributions{rows: []Contribution{{ID: "contribution", DestinationBranchID: fixture.ref.BranchID, Capture: fixture.first, IntegrationSHA: branchPolicyOID(2), Valid: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := plan.Finish(ctx, records, fixture.evidence)
	if !errors.Is(err, context.Canceled) || len(got.Merges) != 1 || got.Merges[0].State != "included" || got.Roots != nil || got.SnapshotIDs == nil || len(got.SnapshotIDs) != 0 || !reflect.DeepEqual(records.positions, []int{0}) || !reflect.DeepEqual(plan.Result(), got) {
		test.Fatalf("cancellation moved before append/inclusion: %+v, %v", got, err)
	}
}

func TestBranchPolicyFinishRelationErrorRetainsPartialMerges(test *testing.T) {
	fixture := newBranchPolicyFixture()
	fixture.evidence.relationError = context.DeadlineExceeded
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(2), branchPolicyInputs(test, fixture), fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
	if !errors.Is(err, context.DeadlineExceeded) || len(got.Merges) != 2 || got.Merges[0].State != "included" || got.Merges[1].State != "review" || !reflect.DeepEqual(got.Roots, []domain.ContentHash{fixture.ref.Target}) || !reflect.DeepEqual(plan.Result(), got) {
		test.Fatalf("relation error lost partial output: %+v, %v", got, err)
	}
}

func TestBranchPolicyMissingGitEvidenceDoesNotProveIntegrationOrder(test *testing.T) {
	for _, relation := range []string{"unknown", "ancestor", "not_ancestor"} {
		test.Run(relation, func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			fixture.evidence.commits = nil
			for _, merge := range []int{2, 3} {
				fixture.evidence.relations[[2]string{branchPolicyOID(merge), branchPolicyOID(4)}] = relation
			}
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), branchPolicyInputs(test, fixture), fixture.evidence)
			if err != nil || len(plan.Result().Merges) != 2 || len(fixture.evidence.treeCalls) != 1 || len(fixture.evidence.relationCalls) != 0 {
				test.Fatalf("missing Git preparation: %+v, %v", plan, err)
			}
			got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
			if err != nil || !reflect.DeepEqual(got.SnapshotIDs, []domain.ContentHash{fixture.ref.Target}) {
				test.Fatalf("unordered ancestry admitted PRs: %+v, %v", got, err)
			}
			wantState, wantReason := "review", "git_evidence_pending"
			if relation == "ancestor" {
				wantReason = "integration_order_unavailable"
			}
			if relation == "not_ancestor" {
				wantState, wantReason = "not_selected", "merge_not_in_selected_code"
			}
			for _, merge := range got.Merges {
				if merge.State != wantState || merge.Reason != wantReason || merge.Order != -1 || !merge.DeliveryAcknowledged {
					test.Fatalf("missing evidence state: %+v", merge)
				}
			}
		})
	}
}

func TestBranchPolicyMissingAndCyclicSnapshotClosures(test *testing.T) {
	for _, scenario := range []string{"missing source", "missing before", "missing natural parent", "natural cycle", "graft cycle"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			inputs := branchPolicyInputs(test, fixture)
			switch scenario {
			case "missing source":
				delete(inputs.snapshots, fixture.first)
			case "missing before":
				delete(inputs.snapshots, fixture.before)
			default:
				fixture.history = nil
				fixture.snapshots[0].Parents = []domain.ContentHash{branchPolicyHash("absent")}
				if scenario != "missing natural parent" {
					fixture.snapshots[0].Parents = []domain.ContentHash{fixture.common}
					fixture.snapshots[4].Parents = []domain.ContentHash{fixture.ref.Target}
					if scenario == "graft cycle" {
						fixture.snapshots[4].Parents = nil
						fixture.snapshots[4].GraftParents = []domain.ContentHash{fixture.ref.Target}
					}
				}
				inputs = branchPolicyInputs(test, fixture)
			}
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), inputs, fixture.evidence)
			if err != nil {
				test.Fatalf("snapshot closure checked before contribution I/O: %v", err)
			}
			got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
			if scenario == "missing source" {
				var unavailable domain.BranchContextMerge
				for _, merge := range got.Merges {
					if merge.Source == fixture.first {
						unavailable = merge
					}
				}
				if err != nil || unavailable.Reason != "source_unavailable" || unavailable.State != "review" || slices.Contains(got.SnapshotIDs, fixture.first) {
					test.Fatalf("missing source changed semantics: %+v, %v", got, err)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("invalid included closure accepted: %+v, %v", got, err)
			}
			if !reflect.DeepEqual(plan.Result(), got) {
				test.Fatalf("finish failure lost partial result: %+v, %+v", plan.Result(), got)
			}
		})
	}
}

func TestBranchPolicyInvalidSnapshotInputs(test *testing.T) {
	fixture := newBranchPolicyFixture()
	foreign := fixture.snapshots[0]
	foreign.RepoID = branchPolicyHash("foreign")
	for _, rows := range [][]domain.Snapshot{{foreign}, {fixture.snapshots[0], fixture.snapshots[0]}} {
		if _, err := NewInputs(context.Background(), fixture.ref.RepoID, rows, nil); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatalf("invalid snapshots admitted: %v", err)
		}
	}
}

func TestBranchPolicyFirstParentTraversalBoundary(test *testing.T) {
	if MaxProjectionSnapshots != 4096 || MaxCodeEvidenceReads != 8192 {
		test.Fatal("projection limits changed", MaxProjectionSnapshots, MaxCodeEvidenceReads)
	}
	for _, scenario := range []string{"terminal", "continuation", "strict continuation"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			fixture.evidence.commits = map[string]domain.GitCommitTree{}
			for number := 1; number <= MaxProjectionSnapshots; number++ {
				commit := domain.GitCommitTree{Commit: branchPolicyOID(number), Parents: []string{}}
				if number < MaxProjectionSnapshots || scenario != "terminal" {
					commit.Parents = []string{branchPolicyOID(number + 1)}
				}
				fixture.evidence.commits[commit.Commit] = commit
			}
			if scenario == "strict continuation" {
				fixture.evidence.exhaustError = domain.ErrMemoryProjectionLimit
			}
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(1), branchPolicyInputs(test, fixture), fixture.evidence)
			if plan == nil || fixture.evidence.reads != MaxProjectionSnapshots || len(fixture.evidence.treeCalls) != MaxProjectionSnapshots {
				test.Fatalf("first-parent bound changed: %+v, %v, reads=%d", plan, err, fixture.evidence.reads)
			}
			wantExhaustions := 1
			if scenario == "terminal" {
				wantExhaustions = 0
			}
			if fixture.evidence.exhaustCalls != wantExhaustions {
				test.Fatalf("traversal exhaustion boundary: %d", fixture.evidence.exhaustCalls)
			}
			if scenario == "strict continuation" {
				if !errors.Is(err, domain.ErrMemoryProjectionLimit) || len(plan.Result().Merges) != 0 {
					test.Fatalf("strict limit crossed receipt boundary: %+v, %v", plan.Result(), err)
				}
			} else if err != nil || len(plan.Result().Merges) != 2 {
				test.Fatalf("terminal/best-effort bound changed: %+v, %v", plan.Result(), err)
			}
		})
	}
}

func TestBranchPolicyUsesSharedCodeEvidenceReadLimit(test *testing.T) {
	for _, strict := range []bool{false, true} {
		test.Run(fmt.Sprintf("strict=%t", strict), func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			fixture.evidence.reads, fixture.evidence.strict = MaxCodeEvidenceReads-1, strict
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(2), branchPolicyInputs(test, fixture), fixture.evidence)
			if plan == nil || fixture.evidence.reads != MaxCodeEvidenceReads || len(fixture.evidence.treeCalls) != 2 || fixture.evidence.exhaustCalls != 0 {
				test.Fatalf("shared 8192 budget bypassed: %+v, %v, reads=%d", plan, err, fixture.evidence.reads)
			}
			if strict {
				if !errors.Is(err, domain.ErrMemoryProjectionLimit) || len(plan.Result().Merges) != 0 {
					test.Fatalf("shared limit did not precede contribution I/O: %+v, %v", plan.Result(), err)
				}
			} else if err != nil || len(plan.Result().Merges) != 2 {
				test.Fatalf("best-effort missing evidence changed: %+v, %v", plan.Result(), err)
			}
		})
	}
}

func TestBranchPolicySnapshotAndInheritanceLimits(test *testing.T) {
	for _, count := range []int{MaxProjectionSnapshots, MaxProjectionSnapshots + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newBranchPolicyFixture()
			fixture.history, fixture.snapshots = nil, nil
			for position := 0; position < count; position++ {
				snapshot := domain.Snapshot{RepoID: fixture.ref.RepoID, ID: branchPolicyHash(fmt.Sprintf("chain-%d", position))}
				if position+1 < count {
					snapshot.Parents = []domain.ContentHash{branchPolicyHash(fmt.Sprintf("chain-%d", position+1))}
				}
				fixture.snapshots = append(fixture.snapshots, snapshot)
			}
			fixture.ref.Target = fixture.snapshots[0].ID
			inputs := branchPolicyInputs(test, fixture)
			plan, err := PrepareBranchContext(context.Background(), fixture.ref, "", inputs, nil)
			if err != nil {
				test.Fatal(err)
			}
			got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, nil)
			if count == MaxProjectionSnapshots {
				if err != nil || len(got.SnapshotIDs) != count || got.Reason != "code_position_unavailable" {
					test.Fatalf("exact closure limit rejected: %d, %v", len(got.SnapshotIDs), err)
				}
			} else if err == nil || err.Error() != "branch context exceeds 4096 snapshots" || got.Roots != nil {
				test.Fatalf("closure overflow changed: %+v, %v", got, err)
			}
			inputs.history.births[fixture.ref.BranchID] = []domain.ContentHash{fixture.ref.Target}
			scopes, err := InheritedIntegrationScopes(context.Background(), fixture.ref.BranchID, inputs.snapshots, inputs.history)
			if count == MaxProjectionSnapshots {
				if err != nil || !reflect.DeepEqual(scopes, map[string]bool{fixture.ref.BranchID: true}) {
					test.Fatalf("exact inheritance limit rejected: %+v, %v", scopes, err)
				}
			} else if err == nil || err.Error() != "branch inheritance exceeds 4096 snapshots" || scopes != nil {
				test.Fatalf("inheritance overflow changed: %+v, %v", scopes, err)
			}
		})
	}
}

func TestBranchPolicyInheritanceCancellationAndIncompleteNaturalAncestry(test *testing.T) {
	fixture := newBranchPolicyFixture()
	inputs := branchPolicyInputs(test, fixture)
	inputs.history.births[fixture.ref.BranchID] = []domain.ContentHash{fixture.first}
	ctx := &branchPolicyCancellationContext{Context: context.Background(), cancelAt: 2}
	if scopes, err := InheritedIntegrationScopes(ctx, fixture.ref.BranchID, inputs.snapshots, inputs.history); scopes != nil || !errors.Is(err, context.Canceled) || ctx.checks != 2 {
		test.Fatalf("inheritance cancellation changed: %+v, %v", scopes, err)
	}
	for _, missing := range []bool{false, true} {
		if missing {
			delete(inputs.snapshots, fixture.common)
		} else {
			snapshot := inputs.snapshots[fixture.common]
			snapshot.Parents = []domain.ContentHash{fixture.first}
			inputs.snapshots[fixture.common] = snapshot
		}
		scopes, err := InheritedIntegrationScopes(context.Background(), fixture.ref.BranchID, inputs.snapshots, inputs.history)
		if err != nil || !reflect.DeepEqual(scopes, map[string]bool{fixture.ref.BranchID: true}) {
			test.Fatalf("inheritance became included-closure validation: %+v, %v", scopes, err)
		}
	}
}

func TestBranchPolicyOrderedSnapshotsPreferCausalityThenRankTimeAndID(test *testing.T) {
	parent, child, sibling := domain.ContentHash("parent"), domain.ContentHash("child"), domain.ContentHash("sibling")
	byID := map[domain.ContentHash]domain.Snapshot{parent: {ID: parent}, child: {ID: child, Parents: []domain.ContentHash{parent}, GraftParents: []domain.ContentHash{parent}}, sibling: {ID: sibling}}
	allowed := map[domain.ContentHash]bool{parent: true, child: true, sibling: true}
	got, err := OrderedBranchSnapshots(byID, allowed, map[domain.ContentHash]int{parent: 0, child: 10, sibling: 1})
	if err != nil || !reflect.DeepEqual(got, []domain.ContentHash{sibling, child, parent}) {
		test.Fatalf("rank overrode causal order: %+v, %v", got, err)
	}
	byID = map[domain.ContentHash]domain.Snapshot{
		"alpha":  {ID: "alpha", CreatedAt: time.Unix(1, 0)},
		"beta":   {ID: "beta", CreatedAt: time.Unix(1, 0)},
		"newest": {ID: "newest", CreatedAt: time.Unix(2, 0)},
	}
	got, err = OrderedBranchSnapshots(byID, map[domain.ContentHash]bool{"alpha": true, "beta": true, "newest": true}, nil)
	if err != nil || !reflect.DeepEqual(got, []domain.ContentHash{"newest", "alpha", "beta"}) {
		test.Fatalf("timestamp/ID fallback changed: %+v, %v", got, err)
	}
}

func TestBranchPolicyExactPublicationOrdersOrdinaryCaptures(test *testing.T) {
	fixture := newBranchPolicyFixture()
	ordinary := branchPolicyHash("ordinary")
	fixture.snapshots[0].GraftParents = []domain.ContentHash{ordinary}
	fixture.snapshots[3].CreatedAt, fixture.snapshots[4].CreatedAt = time.Unix(2, 0), time.Unix(2, 0)
	fixture.snapshots = append(fixture.snapshots, domain.Snapshot{RepoID: fixture.ref.RepoID, ID: ordinary, CreatedAt: time.Unix(1, 0)})
	fixture.history = append(fixture.history,
		domain.HistoryEvent{ID: "ordinary-publication", RepoID: string(fixture.ref.RepoID), Kind: "publish", BranchID: fixture.ref.BranchID, Source: ordinary, Target: ordinary, GitAfter: branchPolicyOID(1)},
		domain.HistoryEvent{ID: "conflicting-publication", RepoID: string(fixture.ref.RepoID), Kind: "publish", BranchID: fixture.ref.BranchID, Source: ordinary, Target: ordinary, GitAfter: branchPolicyOID(4)},
	)
	plan, err := PrepareBranchContext(context.Background(), fixture.ref, branchPolicyOID(4), branchPolicyInputs(test, fixture), fixture.evidence)
	if err != nil {
		test.Fatal(err)
	}
	got, err := plan.Finish(context.Background(), &branchPolicyContributions{}, fixture.evidence)
	if err != nil || !reflect.DeepEqual(got.SnapshotIDs, []domain.ContentHash{fixture.ref.Target, fixture.second, fixture.first, fixture.before, fixture.common, ordinary}) {
		test.Fatalf("ordinary publication lost first association: %+v, %v", got, err)
	}
}
