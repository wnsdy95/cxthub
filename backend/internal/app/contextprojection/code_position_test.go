package contextprojection

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type codePositionOwnerEvidence struct {
	relations     map[[2]string]string
	relationErr   error
	exhaustionErr error
	calls         [][2]string
	exhaustions   int
	treeReads     int
}

func (evidence *codePositionOwnerEvidence) Origin() string {
	return "owner-test-origin"
}

func (evidence *codePositionOwnerEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	evidence.treeReads++
	return domain.GitCommitTree{}, domain.ErrNotFound
}

func (evidence *codePositionOwnerEvidence) Relation(ctx context.Context, source, selected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := [2]string{source, selected}
	evidence.calls = append(evidence.calls, key)
	if evidence.relationErr != nil {
		return "", evidence.relationErr
	}
	if relation, exists := evidence.relations[key]; exists {
		return relation, nil
	}
	if source == selected {
		return "ancestor", nil
	}
	return "unknown", nil
}

func (evidence *codePositionOwnerEvidence) ExhaustTraversal() error {
	evidence.exhaustions++
	return evidence.exhaustionErr
}

func TestCodePositionOwnerCandidateKinds(test *testing.T) {
	ref := domain.Ref{RepoID: historyOwnerHash("candidate-repo"), BranchID: "main-id", Target: historyOwnerHash("candidate-target")}
	for _, kind := range []string{"position", "advance", "birth", "attach", "publish", "orphan", "rename", "archive", "unknown"} {
		test.Run(kind, func(test *testing.T) {
			event := domain.HistoryEvent{Kind: kind, BranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, GitAfter: historyOwnerCode(1)}
			expected := map[string]bool{}
			if slices.Contains([]string{"position", "advance", "birth", "attach", "publish"}, kind) {
				expected[event.GitAfter] = true
			}
			if candidates := BranchCodeCandidates(ref, []domain.HistoryEvent{event, event}); !reflect.DeepEqual(candidates, expected) {
				test.Fatalf("candidates = %v, want %v", candidates, expected)
			}
		})
	}
	for _, event := range []domain.HistoryEvent{
		{Kind: "position", BranchID: "other-id", Target: ref.Target, GitAfter: historyOwnerCode(1)},
		{Kind: "position", BranchID: ref.BranchID, Target: historyOwnerHash("other"), GitAfter: historyOwnerCode(1)},
		{Kind: "publish", BranchID: ref.BranchID, Source: historyOwnerHash("other"), Target: ref.Target, GitAfter: historyOwnerCode(1)},
		{Kind: "position", BranchID: ref.BranchID, Target: ref.Target, GitAfter: "invalid"},
		{Kind: "position", BranchID: ref.BranchID, Target: ref.Target},
	} {
		if candidates := BranchCodeCandidates(ref, []domain.HistoryEvent{event}); len(candidates) != 0 || BranchCode(ref, []domain.HistoryEvent{event}) != "" {
			test.Fatalf("unrelated or invalid association selected: %+v/%v", event, candidates)
		}
	}
}

func TestCodePositionOwnerTrackingAlias(test *testing.T) {
	ref := domain.Ref{RepoID: historyOwnerHash("alias-repo"), BranchID: "main-id", Target: historyOwnerHash("alias-target")}
	publication := domain.HistoryEvent{Kind: "publish", BranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, GitAfter: historyOwnerCode(9)}
	completion := domain.HistoryEvent{Kind: "pr-merge", BranchID: ref.BranchID, Source: ref.Target, Target: ref.Target, PRCompleted: true, PR: &domain.PullRequestMerge{HeadSHA: historyOwnerCode(9), MergeSHA: historyOwnerCode(3)}}
	for _, name := range []string{"completed", "reversed", "incomplete", "different identity", "different target", "different source", "unrelated position", "competing completions", "invalid merge", "nil PR", "unchanged code"} {
		test.Run(name, func(test *testing.T) {
			history := []domain.HistoryEvent{publication, completion}
			expected := map[string]bool{historyOwnerCode(3): true}
			switch name {
			case "reversed":
				slices.Reverse(history)
			case "incomplete":
				history[1].PRCompleted = false
				expected = map[string]bool{historyOwnerCode(9): true}
			case "different identity":
				history[1].BranchID = "recreated-main-id"
				expected = map[string]bool{historyOwnerCode(9): true}
			case "different target":
				history[1].Target = historyOwnerHash("other-target")
				expected = map[string]bool{historyOwnerCode(9): true}
			case "different source":
				history[1].Source = historyOwnerHash("other-source")
				expected[historyOwnerCode(9)] = true
			case "unrelated position":
				history = append(history, domain.HistoryEvent{Kind: "position", BranchID: ref.BranchID, Target: ref.Target, GitAfter: historyOwnerCode(8)})
				expected[historyOwnerCode(8)] = true
			case "competing completions":
				other := completion
				other.PR = &domain.PullRequestMerge{HeadSHA: historyOwnerCode(9), MergeSHA: historyOwnerCode(4)}
				history = append(history, other)
				expected[historyOwnerCode(4)] = true
			case "invalid merge":
				history[1].PR = &domain.PullRequestMerge{HeadSHA: historyOwnerCode(9), MergeSHA: "invalid"}
				expected = map[string]bool{historyOwnerCode(9): true}
			case "nil PR":
				history[1].PR = nil
				expected = map[string]bool{historyOwnerCode(9): true}
			case "unchanged code":
				history[1].PR = &domain.PullRequestMerge{HeadSHA: historyOwnerCode(9), MergeSHA: historyOwnerCode(9)}
				expected = map[string]bool{historyOwnerCode(9): true}
			}
			if candidates := BranchCodeCandidates(ref, history); !reflect.DeepEqual(candidates, expected) {
				test.Fatalf("candidates = %v, want %v", candidates, expected)
			}
			selected := ""
			if len(expected) == 1 {
				for code := range expected {
					selected = code
				}
			}
			if code := BranchCode(ref, history); code != selected {
				test.Fatalf("code = %q, want %q", code, selected)
			}
		})
	}
}

func codePositionOwnerCausalFixture() (domain.Ref, []domain.HistoryEvent) {
	repo := historyOwnerHash("causal-repo")
	source, retained := historyOwnerHash("causal-source"), historyOwnerHash("causal-retained")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: retained}
	first := domain.HistoryEvent{ID: "first-receipt", RepoID: string(repo), Kind: "pr-merge", BranchID: ref.BranchID, SourceBranchID: "first-feature", Source: source, Target: source, PR: &domain.PullRequestMerge{Number: 1, HeadSHA: historyOwnerCode(8), MergeSHA: historyOwnerCode(2)}}
	firstCompletion := historyOwnerCompletion(first)
	firstCompletion.Target, firstCompletion.SharedTarget = retained, retained
	second := domain.HistoryEvent{ID: "second-receipt", RepoID: string(repo), Kind: "pr-merge", BranchID: ref.BranchID, SourceBranchID: "second-feature", Source: retained, Target: retained, PR: &domain.PullRequestMerge{Number: 2, HeadSHA: historyOwnerCode(9), MergeSHA: historyOwnerCode(3)}}
	secondCompletion := historyOwnerCompletion(second)
	secondCompletion.SharedTarget = source
	observation := domain.HistoryEvent{ID: "source-observation", RepoID: string(repo), Kind: "position", BranchID: ref.BranchID, Target: source, GitAfter: historyOwnerCode(8)}
	return ref, []domain.HistoryEvent{first, firstCompletion, second, secondCompletion, observation}
}

func TestCodePositionOwnerCausalSquash(test *testing.T) {
	for _, name := range []string{"contained source", "reversed history", "unrelated older code", "different source", "different destination", "missing receipt", "missing completion", "historical source", "future completion", "same code unrelated context", "valid before conflict", "conflict before valid"} {
		test.Run(name, func(test *testing.T) {
			ref, history := codePositionOwnerCausalFixture()
			selected := historyOwnerCode(3)
			var wantErr error
			switch name {
			case "reversed history":
				slices.Reverse(history)
			case "unrelated older code":
				history[4].GitAfter = historyOwnerCode(9)
				selected = ""
			case "different source":
				history[0].Source, history[1].Source = historyOwnerHash("other-source"), historyOwnerHash("other-source")
				selected = ""
			case "different destination":
				history[0].BranchID, history[1].BranchID = "other-destination", "other-destination"
				selected = ""
			case "missing receipt":
				history = slices.Delete(history, 0, 1)
				selected = ""
			case "missing completion":
				history = slices.Delete(history, 1, 2)
				selected = ""
			case "historical source":
				ref.Target = history[0].Source
				selected = historyOwnerCode(8)
			case "future completion":
				future := history[0]
				future.ID = "future-receipt"
				futureCompletion := historyOwnerCompletion(future)
				futureCompletion.Target = historyOwnerHash("future-target")
				history = append(slices.Clone(history[1:]), future, futureCompletion)
				selected = ""
			case "same code unrelated context":
				other := historyOwnerHash("unrelated-context")
				history = append(history,
					domain.HistoryEvent{ID: "other-completion", RepoID: string(ref.RepoID), BranchID: ref.BranchID, Kind: "pr-merge", Target: ref.Target, SharedTarget: other, PRCompleted: true, PR: &domain.PullRequestMerge{MergeSHA: historyOwnerCode(3)}},
					domain.HistoryEvent{ID: "other-observation", RepoID: string(ref.RepoID), BranchID: ref.BranchID, Kind: "position", Target: other, GitAfter: historyOwnerCode(8)},
				)
				selected = ""
			case "valid before conflict", "conflict before valid":
				conflict := history[1]
				conflict.Source = historyOwnerHash("conflicting-source")
				if name == "valid before conflict" {
					history = append(history, conflict)
				} else {
					history = append([]domain.HistoryEvent{conflict}, history...)
					selected, wantErr = "", domain.ErrRefConflict
				}
			}
			index, err := NewHistoryIndex(context.Background(), ref.RepoID, history)
			if err != nil {
				test.Fatal(err)
			}
			for _, indexed := range []bool{false, true} {
				evidence := &codePositionOwnerEvidence{relations: map[[2]string]string{{historyOwnerCode(2), historyOwnerCode(3)}: "ancestor"}}
				var code string
				if indexed {
					code, err = ResolveBranchCodeFromIndex(context.Background(), ref, index, evidence)
				} else {
					code, err = ResolveBranchCode(context.Background(), ref, history, evidence)
				}
				if code != selected || !errors.Is(err, wantErr) || evidence.treeReads != 0 {
					test.Fatalf("indexed=%t: code = %q/%v, want %q/%v; tree reads=%d", indexed, code, err, selected, wantErr, evidence.treeReads)
				}
			}
		})
	}
}

func TestCodePositionOwnerOrdinaryAndAncestryRules(test *testing.T) {
	for _, name := range []string{"ancestral winner", "older ordinary ambiguity", "newer ordinary", "newer publication", "ordinary reset", "ordinary merged reset", "ordinary merged winner", "ordinary conflict", "unrelated identity", "unknown ancestry", "not ancestor", "no evidence"} {
		test.Run(name, func(test *testing.T) {
			repo := historyOwnerHash("ordinary-repo")
			current, older, base := historyOwnerHash("ordinary-current"), historyOwnerHash("ordinary-older"), historyOwnerHash("ordinary-base")
			ref := domain.Ref{RepoID: repo, BranchID: "main-id", Target: current}
			history := []domain.HistoryEvent{
				{ID: "current", RepoID: string(repo), BranchID: ref.BranchID, Kind: "pr-merge", Target: current, SharedTarget: older, PRCompleted: true, PR: &domain.PullRequestMerge{MergeSHA: historyOwnerCode(2)}},
				{ID: "older", RepoID: string(repo), BranchID: ref.BranchID, Kind: "pr-merge", Target: older, SharedTarget: base, PRCompleted: true, PR: &domain.PullRequestMerge{MergeSHA: historyOwnerCode(3)}},
				{ID: "base", RepoID: string(repo), BranchID: ref.BranchID, Kind: "position", Target: base, GitAfter: historyOwnerCode(1)},
			}
			evidence := &codePositionOwnerEvidence{relations: map[[2]string]string{
				{historyOwnerCode(1), historyOwnerCode(2)}: "ancestor",
				{historyOwnerCode(1), historyOwnerCode(3)}: "ancestor",
				{historyOwnerCode(2), historyOwnerCode(3)}: "ancestor",
				{historyOwnerCode(1), historyOwnerCode(4)}: "ancestor",
				{historyOwnerCode(2), historyOwnerCode(4)}: "ancestor",
				{historyOwnerCode(3), historyOwnerCode(4)}: "ancestor",
			}}
			selected := historyOwnerCode(3)
			ordinary := domain.HistoryEvent{ID: "ordinary", RepoID: string(repo), BranchID: ref.BranchID, Kind: "position", Source: current, Target: current}
			switch name {
			case "older ordinary ambiguity":
				for _, number := range []int{1, 2} {
					observation := ordinary
					observation.Target, observation.GitAfter = base, historyOwnerCode(number)
					history = append(history, observation)
				}
			case "newer ordinary", "newer publication":
				ordinary.GitAfter = historyOwnerCode(4)
				if name == "newer publication" {
					ordinary.Kind = "publish"
				}
				history = append(history, ordinary)
				selected = historyOwnerCode(4)
			case "ordinary reset":
				ordinary.GitAfter = historyOwnerCode(1)
				history = append(history, ordinary)
				selected = ""
			case "ordinary merged reset":
				ordinary.GitAfter = historyOwnerCode(2)
				history = append(history, ordinary)
				selected = ""
			case "ordinary merged winner":
				ordinary.GitAfter = historyOwnerCode(3)
				history = append(history, ordinary)
			case "ordinary conflict":
				for _, number := range []int{2, 3} {
					observation := ordinary
					observation.GitAfter = historyOwnerCode(number)
					history = append(history, observation)
				}
				selected = ""
			case "unrelated identity":
				ordinary.BranchID, ordinary.GitAfter = "recreated-main-id", historyOwnerCode(4)
				history = append(history, ordinary)
			case "unknown ancestry", "not ancestor":
				relation := "unknown"
				if name == "not ancestor" {
					relation = "not_ancestor"
				}
				evidence.relations[[2]string{historyOwnerCode(1), historyOwnerCode(3)}] = relation
				selected = ""
			case "no evidence":
				evidence = nil
				selected = historyOwnerCode(2)
			}
			var contract CodeEvidence
			if evidence != nil {
				contract = evidence
			}
			code, err := ResolveBranchCode(context.Background(), ref, history, contract)
			if err != nil || code != selected {
				test.Fatalf("code = %q/%v, want %q", code, err, selected)
			}
			if name == "ordinary conflict" && len(evidence.calls) != 0 {
				test.Fatalf("ancestry was used to resolve ordinary ambiguity: %+v", evidence.calls)
			}
		})
	}
}

func TestCodePositionOwnerSharedIndexKeepsSelectionsSeparate(test *testing.T) {
	ref, history := codePositionOwnerCausalFixture()
	source, reset, ambiguous := history[0].Source, historyOwnerHash("reset"), historyOwnerHash("ambiguous")
	for position, observation := range []struct {
		branch string
		target domain.ContentHash
		code   int
	}{
		{ref.BranchID, reset, 1},
		{ref.BranchID, ambiguous, 2},
		{ref.BranchID, ambiguous, 3},
		{"recreated-main-id", ref.Target, 5},
	} {
		history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("extra-%d", position), RepoID: string(ref.RepoID), BranchID: observation.branch, Kind: "position", Target: observation.target, GitAfter: historyOwnerCode(observation.code)})
	}
	index, err := NewHistoryIndex(context.Background(), ref.RepoID, history)
	if err != nil {
		test.Fatal(err)
	}
	evidence := &codePositionOwnerEvidence{relations: map[[2]string]string{{historyOwnerCode(2), historyOwnerCode(3)}: "ancestor"}}
	for _, selection := range []struct {
		branch string
		target domain.ContentHash
		code   string
	}{
		{ref.BranchID, ref.Target, historyOwnerCode(3)},
		{ref.BranchID, source, historyOwnerCode(8)},
		{ref.BranchID, ambiguous, ""},
		{ref.BranchID, reset, historyOwnerCode(1)},
		{"recreated-main-id", ref.Target, historyOwnerCode(5)},
		{ref.BranchID, ref.Target, historyOwnerCode(3)},
	} {
		selected := ref
		selected.BranchID, selected.Target = selection.branch, selection.target
		code, err := ResolveBranchCodeFromIndex(context.Background(), selected, index, evidence)
		if err != nil || code != selection.code {
			test.Fatalf("selection %+v = %q/%v, want %q", selected, code, err, selection.code)
		}
	}
	if !reflect.DeepEqual(index.Rows(), history) {
		test.Fatal("resolving a selection changed history")
	}
}

func TestCodePositionOwnerNilEvidenceAndEmptySelection(test *testing.T) {
	ref, history := codePositionOwnerCausalFixture()
	index, err := NewHistoryIndex(context.Background(), ref.RepoID, history)
	if err != nil {
		test.Fatal(err)
	}
	ctx := historyOwnerCancelAt(test, 2)
	if code, err := ResolveBranchCodeFromIndex(ctx, ref, index, nil); code != "" || err != nil || ctx.checks != 1 {
		test.Fatalf("nil evidence performed a causal walk: %q/%v after %d checks", code, err, ctx.checks)
	}
	ref.Target = history[0].Source
	ctx = historyOwnerCancelAt(test, 2)
	if code, err := ResolveBranchCodeFromIndex(ctx, ref, index, nil); code != historyOwnerCode(8) || err != nil || ctx.checks != 1 {
		test.Fatalf("nil evidence changed exact selection: %q/%v after %d checks", code, err, ctx.checks)
	}
	ref.Target = historyOwnerHash("unobserved-target")
	evidence := &codePositionOwnerEvidence{}
	if code, err := ResolveBranchCodeFromIndex(context.Background(), ref, index, evidence); code != "" || err != nil || len(evidence.calls) != 0 {
		test.Fatalf("empty selection fabricated evidence: %q/%v, calls=%v", code, err, evidence.calls)
	}
}

func TestCodePositionOwnerAncestryOrderIsDeterministic(test *testing.T) {
	ref, history := codePositionOwnerCausalFixture()
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(history)
		}
		evidence := &codePositionOwnerEvidence{relations: map[[2]string]string{{historyOwnerCode(2), historyOwnerCode(3)}: "ancestor"}}
		code, err := ResolveBranchCode(context.Background(), ref, history, evidence)
		expected := [][2]string{
			{historyOwnerCode(2), historyOwnerCode(3)},
			{historyOwnerCode(2), historyOwnerCode(3)},
			{historyOwnerCode(3), historyOwnerCode(3)},
		}
		if code != historyOwnerCode(3) || err != nil || !reflect.DeepEqual(evidence.calls, expected) {
			test.Fatalf("reversed=%t: %q/%v, calls=%v, want %v", reverse, code, err, evidence.calls, expected)
		}
	}
}

func codePositionOwnerChain(count int) (domain.Ref, []domain.HistoryEvent) {
	repo := historyOwnerHash("chain-repo")
	targets := make([]domain.ContentHash, count)
	for position := range targets {
		targets[position] = historyOwnerHash(fmt.Sprintf("chain-target-%d", position))
	}
	ref := domain.Ref{RepoID: repo, BranchID: "main-id", Target: targets[0]}
	history := make([]domain.HistoryEvent, count)
	for position, target := range targets {
		event := domain.HistoryEvent{ID: fmt.Sprintf("%032x", position+1), RepoID: string(repo), BranchID: ref.BranchID, Kind: "position", Target: target, GitAfter: historyOwnerCode(1)}
		if position+1 < count {
			event.Kind, event.PRCompleted, event.SharedTarget = "pr-merge", true, targets[position+1]
			event.PR = &domain.PullRequestMerge{MergeSHA: historyOwnerCode(1)}
		}
		history[position] = event
	}
	return ref, history
}

func TestCodePositionOwnerTraversalLimit(test *testing.T) {
	if MaxProjectionSnapshots != 4096 || MaxCodeEvidenceReads != 8192 {
		test.Fatal("projection contract limits changed")
	}
	for _, scenario := range []struct {
		name          string
		count         int
		exhaustionErr error
		selected      string
		wantErr       error
		exhaustions   int
	}{
		{"beyond snapshot bound", MaxProjectionSnapshots + 1, nil, historyOwnerCode(1), nil, 0},
		{"exact evidence bound", MaxCodeEvidenceReads, nil, historyOwnerCode(1), nil, 0},
		{"legacy exhaustion", MaxCodeEvidenceReads + 1, nil, "", nil, 1},
		{"strict exhaustion", MaxCodeEvidenceReads + 1, domain.ErrMemoryProjectionLimit, "", domain.ErrMemoryProjectionLimit, 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ref, history := codePositionOwnerChain(scenario.count)
			evidence := &codePositionOwnerEvidence{exhaustionErr: scenario.exhaustionErr}
			code, err := ResolveBranchCode(context.Background(), ref, history, evidence)
			if code != scenario.selected || !errors.Is(err, scenario.wantErr) || evidence.exhaustions != scenario.exhaustions {
				test.Fatalf("code = %q/%v after %d exhaustions, want %q/%v after %d", code, err, evidence.exhaustions, scenario.selected, scenario.wantErr, scenario.exhaustions)
			}
			if scenario.exhaustions != 0 && len(evidence.calls) != 0 {
				test.Fatal("partial traversal reached ancestry selection")
			}
		})
	}
}

func TestCodePositionOwnerCyclesAndRepeatedTargets(test *testing.T) {
	for _, name := range []string{"cycle", "self target", "repeated target"} {
		test.Run(name, func(test *testing.T) {
			ref, history := codePositionOwnerChain(3)
			selected := historyOwnerCode(1)
			switch name {
			case "cycle":
				history[1].SharedTarget = ref.Target
				selected = ""
			case "self target":
				history[0].SharedTarget = ref.Target
			case "repeated target":
				repeated := history[0]
				repeated.ID = "repeated"
				history = append(history, repeated)
			}
			evidence := &codePositionOwnerEvidence{}
			code, err := ResolveBranchCode(context.Background(), ref, history, evidence)
			if err != nil || code != selected || evidence.exhaustions != 0 {
				test.Fatalf("code = %q/%v, want %q without exhaustion", code, err, selected)
			}
		})
	}
}

func TestCodePositionOwnerCancellationAndErrors(test *testing.T) {
	ref, history := codePositionOwnerCausalFixture()
	index, err := NewHistoryIndex(context.Background(), ref.RepoID, history)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelAt := range []int{1, 2, 3, 4} {
		test.Run(fmt.Sprintf("cancel-at-%d", cancelAt), func(test *testing.T) {
			ctx := historyOwnerCancelAt(test, cancelAt)
			evidence := &codePositionOwnerEvidence{}
			code, err := ResolveBranchCodeFromIndex(ctx, ref, index, evidence)
			if code != "" || !errors.Is(err, context.Canceled) || ctx.checks != cancelAt {
				test.Fatalf("cancellation = %q/%v at check %d, want %d", code, err, ctx.checks, cancelAt)
			}
		})
	}
	for _, wantErr := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrMemoryProjectionLimit, context.DeadlineExceeded} {
		evidence := &codePositionOwnerEvidence{relationErr: wantErr}
		if code, err := ResolveBranchCodeFromIndex(context.Background(), ref, index, evidence); code != "" || !errors.Is(err, wantErr) {
			test.Fatalf("relation error = %q/%v, want %v", code, err, wantErr)
		}
	}
	foreign := ref
	foreign.RepoID = historyOwnerHash("foreign-repo")
	if code, err := ResolveBranchCodeFromIndex(context.Background(), foreign, index, nil); code != "" || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatalf("foreign index accepted: %q/%v", code, err)
	}
	if code, err := ResolveBranchCode(context.Background(), foreign, history, nil); code != "" || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatalf("foreign history accepted: %q/%v", code, err)
	}
	ctx := historyOwnerCancelAt(test, 1)
	if code, err := ResolveBranchCodeFromIndex(ctx, foreign, index, nil); code != "" || !errors.Is(err, context.Canceled) {
		test.Fatalf("integrity overtook resolver cancellation: %q/%v", code, err)
	}
	ctx = historyOwnerCancelAt(test, 1)
	if code, err := ResolveBranchCode(ctx, foreign, history, nil); code != "" || !errors.Is(err, context.Canceled) {
		test.Fatalf("integrity overtook history cancellation: %q/%v", code, err)
	}
	longRef, longHistory := codePositionOwnerChain(MaxCodeEvidenceReads + 1)
	longIndex, err := NewHistoryIndex(context.Background(), longRef.RepoID, longHistory)
	if err != nil {
		test.Fatal(err)
	}
	ctx = historyOwnerCancelAt(test, MaxCodeEvidenceReads+2)
	evidence := &codePositionOwnerEvidence{exhaustionErr: domain.ErrMemoryProjectionLimit}
	if code, err := ResolveBranchCodeFromIndex(ctx, longRef, longIndex, evidence); code != "" || !errors.Is(err, context.Canceled) || evidence.exhaustions != 0 {
		test.Fatalf("limit overtook traversal cancellation: %q/%v, exhaustions=%d", code, err, evidence.exhaustions)
	}
}
