package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type introductionTreeReader struct {
	commits map[string]domain.GitCommitTree
	reads   int
	onRead  func(string)
}

func (reader *introductionTreeReader) GetGitCommitTree(_ context.Context, _ domain.ContentHash, _, commit string) (domain.GitCommitTree, error) {
	reader.reads++
	if reader.onRead != nil {
		reader.onRead(commit)
	}
	proof, exists := reader.commits[commit]
	if !exists {
		return proof, domain.ErrNotFound
	}
	return proof, nil
}

func (*introductionTreeReader) GetGitTreeNode(context.Context, domain.ContentHash, string, string) (domain.GitTreeNode, error) {
	return domain.GitTreeNode{}, domain.ErrNotFound
}

func introductionCommit(number int, parents ...int) domain.GitCommitTree {
	proof := domain.GitCommitTree{Commit: fmt.Sprintf("%040x", number), Tree: fmt.Sprintf("%040x", 99999), Parents: []string{}}
	for _, parent := range parents {
		proof.Parents = append(proof.Parents, fmt.Sprintf("%040x", parent))
	}
	return proof
}

func introductionEvidence(commits ...domain.GitCommitTree) (*codeEvidence, *introductionTreeReader) {
	reader := &introductionTreeReader{commits: map[string]domain.GitCommitTree{}}
	for _, commit := range commits {
		reader.commits[commit.Commit] = commit
	}
	return &codeEvidence{repo: hh("introduction repo"), origin: "https://github.com/acme/repo", trees: reader}, reader
}

func TestGitContributionIntroductionUsesActualFirstParentBoundary(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		source int
		head   int
		want   int
	}{
		{"side branch merge not later tip", 3, 6, 4},
		{"direct first-parent commit", 2, 6, 2},
		{"merge commit itself", 4, 6, 4},
		{"root contribution", 1, 6, 1},
		{"current tip", 6, 6, 6},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence, reader := introductionEvidence(introductionCommit(1), introductionCommit(2, 1), introductionCommit(3, 1), introductionCommit(4, 2, 3), introductionCommit(5, 4), introductionCommit(6, 5, 3))
			got, err := locateGitContributionIntroduction(context.Background(), evidence, introductionCommit(scenario.source).Commit, introductionCommit(scenario.head).Commit)
			if err != nil || got != introductionCommit(scenario.want).Commit || reader.reads > len(reader.commits) {
				t.Fatalf("introduction=%s want=%d reads=%d err=%v", got, scenario.want, reader.reads, err)
			}
		})
	}
}

func TestGitContributionIntroductionUnknownIsNotAbsence(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		commits []domain.GitCommitTree
		want    error
	}{
		{"missing before boundary", []domain.GitCommitTree{introductionCommit(1), introductionCommit(3, 1), introductionCommit(4, 2, 3)}, errGitContributionEvidencePending},
		{"missing source", []domain.GitCommitTree{introductionCommit(1), introductionCommit(2, 1), introductionCommit(4, 2, 3)}, errGitContributionEvidencePending},
		{"missing destination", []domain.GitCommitTree{introductionCommit(3)}, errGitContributionEvidencePending},
		{"complete unrelated history", []domain.GitCommitTree{introductionCommit(1), introductionCommit(2, 1), introductionCommit(3), introductionCommit(4, 2)}, domain.ErrNotFound},
		{"cyclic before boundary", []domain.GitCommitTree{introductionCommit(2, 4), introductionCommit(3), introductionCommit(4, 2, 3)}, domain.ErrIntegrity},
		{"cycle through source", []domain.GitCommitTree{introductionCommit(2), introductionCommit(3, 4), introductionCommit(4, 2, 3)}, domain.ErrIntegrity},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence, _ := introductionEvidence(scenario.commits...)
			got, err := locateGitContributionIntroduction(context.Background(), evidence, introductionCommit(3).Commit, introductionCommit(4).Commit)
			if got != "" || !errors.Is(err, scenario.want) {
				t.Fatalf("unknown introduction accepted: %s err=%v want=%v", got, err, scenario.want)
			}
		})
	}
}

func TestGitContributionIntroductionBoundsAndCancellation(t *testing.T) {
	commits := []domain.GitCommitTree{introductionCommit(1)}
	for number := 2; number < maxCodeEvidenceReads+5; number++ {
		commits = append(commits, introductionCommit(number, number+1))
	}
	destination := introductionCommit(maxCodeEvidenceReads+10, 2, 1)
	commits = append(commits, destination)
	evidence, reader := introductionEvidence(commits...)
	if got, err := locateGitContributionIntroduction(context.Background(), evidence, introductionCommit(1).Commit, destination.Commit); got != "" || !errors.Is(err, errGitContributionEvidenceLimit) || reader.reads > maxCodeEvidenceReads {
		t.Fatalf("truncated first parent treated as absent: %s reads=%d err=%v", got, reader.reads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := reader.reads
	if _, err := locateGitContributionIntroduction(ctx, evidence, introductionCommit(1).Commit, destination.Commit); !errors.Is(err, context.Canceled) || reader.reads != before {
		t.Fatalf("canceled discovery read storage: reads=%d err=%v", reader.reads, err)
	}
}

func TestGitContributionRangeRequiresNewContinuousIntroduction(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		source  int
		before  int
		after   int
		want    int
		wantErr error
	}{
		{"merge boundary not latest head", 3, 2, 6, 4, nil},
		{"fast forward is valid", 5, 4, 6, 5, nil},
		{"source tip is the fast forward", 6, 5, 6, 6, nil},
		{"branch born at main commit", 2, 2, 6, 0, errGitContributionAlreadyPresent},
		{"earlier join is not a new join", 3, 4, 6, 0, errGitContributionAlreadyPresent},
		{"unchanged head", 3, 2, 2, 0, domain.ErrNotFound},
		{"source not integrated", 7, 2, 6, 0, domain.ErrNotFound},
		{"destination reset to different lineage", 3, 7, 6, 0, errGitContributionRangeDiscontinuous},
		{"baseline only a secondary parent", 2, 3, 6, 0, errGitContributionRangeDiscontinuous},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence, reader := introductionEvidence(introductionCommit(1), introductionCommit(2, 1), introductionCommit(3, 1), introductionCommit(4, 2, 3), introductionCommit(5, 4), introductionCommit(6, 5), introductionCommit(7, 1))
			got, err := locateGitContributionInRange(context.Background(), evidence, introductionCommit(scenario.source).Commit, introductionCommit(scenario.before).Commit, introductionCommit(scenario.after).Commit)
			want := ""
			if scenario.want != 0 {
				want = introductionCommit(scenario.want).Commit
			}
			if got != want || !errors.Is(err, scenario.wantErr) || reader.reads > len(reader.commits) {
				t.Fatalf("introduction=%s want=%s err=%v wantErr=%v reads=%d", got, want, err, scenario.wantErr, reader.reads)
			}
		})
	}
}

func TestGitContributionRangeWithholdsIncompleteBaseline(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		commits []domain.GitCommitTree
		want    error
	}{
		{"baseline missing", []domain.GitCommitTree{introductionCommit(1), introductionCommit(3, 1), introductionCommit(4, 2, 3)}, errGitContributionEvidencePending},
		{"baseline ancestor missing", []domain.GitCommitTree{introductionCommit(2, 1), introductionCommit(3), introductionCommit(4, 2, 3)}, errGitContributionEvidencePending},
		{"cyclic baseline", []domain.GitCommitTree{introductionCommit(1, 2), introductionCommit(2, 1), introductionCommit(3), introductionCommit(4, 2, 3)}, domain.ErrIntegrity},
		{"cycle through source after baseline", []domain.GitCommitTree{introductionCommit(1), introductionCommit(2, 1), introductionCommit(3, 4), introductionCommit(4, 2, 3)}, domain.ErrIntegrity},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence, _ := introductionEvidence(scenario.commits...)
			got, err := locateGitContributionInRange(context.Background(), evidence, introductionCommit(3).Commit, introductionCommit(2).Commit, introductionCommit(4).Commit)
			if got != "" || !errors.Is(err, scenario.want) {
				t.Fatalf("unproven baseline accepted: %s %v", got, err)
			}
		})
	}
	evidence, reader := introductionEvidence()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locateGitContributionInRange(ctx, evidence, "", "", ""); !errors.Is(err, context.Canceled) || reader.reads != 0 {
		t.Fatalf("canceled range read evidence: %v reads=%d", err, reader.reads)
	}
	if _, err := locateGitContributionInRange(context.Background(), evidence, introductionCommit(3).Commit, "", introductionCommit(4).Commit); !errors.Is(err, domain.ErrValidation) || reader.reads != 0 {
		t.Fatalf("missing baseline read evidence: %v reads=%d", err, reader.reads)
	}
}

func TestGitContributionRangeSharesEvidenceBudget(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		baselineLength int
		wantErr        error
	}{
		{"complete at shared limit", maxCodeEvidenceReads - 3, nil},
		{"after first parent exceeds shared limit", maxCodeEvidenceReads - 2, errGitContributionEvidenceLimit},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			commits := []domain.GitCommitTree{introductionCommit(1)}
			for number := 2; number <= scenario.baselineLength; number++ {
				commits = append(commits, introductionCommit(number, number-1))
			}
			source := introductionCommit(maxCodeEvidenceReads + 1)
			introduction := introductionCommit(maxCodeEvidenceReads+2, scenario.baselineLength, maxCodeEvidenceReads+1)
			after := introductionCommit(maxCodeEvidenceReads+3, maxCodeEvidenceReads+2)
			commits = append(commits, source, introduction, after)
			evidence, reader := introductionEvidence(commits...)
			readsBeforeAfter := 0
			reader.onRead = func(commit string) {
				if commit == after.Commit {
					readsBeforeAfter = reader.reads - 1
				}
			}
			got, err := locateGitContributionInRange(context.Background(), evidence, source.Commit, introductionCommit(scenario.baselineLength).Commit, after.Commit)
			want := introduction.Commit
			if scenario.wantErr != nil {
				want = ""
			}
			if got != want || !errors.Is(err, scenario.wantErr) {
				t.Fatalf("introduction=%s want=%s err=%v wantErr=%v", got, want, err, scenario.wantErr)
			}
			if readsBeforeAfter != scenario.baselineLength+1 || reader.reads != maxCodeEvidenceReads || evidence.ReadCount() != maxCodeEvidenceReads || evidence.Limited() != (scenario.wantErr != nil) {
				t.Fatalf("range did not share evidence budget: beforeAfter=%d reads=%d budgetReads=%d limited=%v", readsBeforeAfter, reader.reads, evidence.ReadCount(), evidence.Limited())
			}
		})
	}
}

func TestGitContributionRangeCancelsDuringTraversal(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		cancelAt int
		reads    int
	}{
		{"before first parent", 2, 2},
		{"after first parent", 4, 5},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence, reader := introductionEvidence(introductionCommit(1), introductionCommit(2, 1), introductionCommit(3, 1), introductionCommit(4, 2, 3), introductionCommit(5, 4))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			readsAtCancel := 0
			reader.onRead = func(commit string) {
				if commit == introductionCommit(scenario.cancelAt).Commit {
					readsAtCancel = reader.reads
					cancel()
				}
			}
			got, err := locateGitContributionInRange(ctx, evidence, introductionCommit(3).Commit, introductionCommit(2).Commit, introductionCommit(5).Commit)
			if got != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled traversal accepted: introduction=%s err=%v", got, err)
			}
			if readsAtCancel != scenario.reads || reader.reads != readsAtCancel {
				t.Fatalf("traversal read beyond cancellation: canceledAt=%d reads=%d want=%d", readsAtCancel, reader.reads, scenario.reads)
			}
		})
	}
}

func TestGitContributionRangeWithholdsIncompleteAfter(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		commits []domain.GitCommitTree
		after   int
		want    int
	}{
		{"missing immediate first parent", []domain.GitCommitTree{introductionCommit(3, 1), introductionCommit(5, 4, 3)}, 5, 5},
		{"missing deeper first parent", []domain.GitCommitTree{introductionCommit(3, 1), introductionCommit(5, 4, 3), introductionCommit(6, 5)}, 6, 5},
		{"missing continuity below source tip", []domain.GitCommitTree{introductionCommit(3, 4)}, 3, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			commits := append([]domain.GitCommitTree{introductionCommit(1), introductionCommit(2, 1)}, scenario.commits...)
			evidence, _ := introductionEvidence(commits...)
			got, err := locateGitContributionInRange(context.Background(), evidence, introductionCommit(3).Commit, introductionCommit(2).Commit, introductionCommit(scenario.after).Commit)
			if got != "" || !errors.Is(err, errGitContributionEvidencePending) || errors.Is(err, errGitContributionEvidenceLimit) || evidence.Limited() {
				t.Fatalf("incomplete after evidence accepted: introduction=%s err=%v limited=%v", got, err, evidence.Limited())
			}
			_, cachedErr, checked := evidence.CachedCommit(introductionCommit(4).Commit)
			if !checked || !errors.Is(cachedErr, domain.ErrNotFound) {
				t.Fatalf("missing first-parent evidence was not checked: checked=%v err=%v", checked, cachedErr)
			}
			commits = append(commits, introductionCommit(4, 2))
			completeEvidence, _ := introductionEvidence(commits...)
			got, err = locateGitContributionInRange(context.Background(), completeEvidence, introductionCommit(3).Commit, introductionCommit(2).Commit, introductionCommit(scenario.after).Commit)
			if got != introductionCommit(scenario.want).Commit || err != nil {
				t.Fatalf("complete first-parent range rejected: introduction=%s want=%d err=%v", got, scenario.want, err)
			}
		})
	}
}
