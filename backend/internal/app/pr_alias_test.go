package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRTrackingAliasUsesExactHeadAndCanonicalIdentity(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("tracking-pr")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/a/b"}); err != nil {
		t.Fatal(err)
	}
	base := prSnapshot(t, st, repo, "base")
	source := prSnapshot(t, st, repo, "exact alias context", base)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		t.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 156, BaseBranch: "main", HeadBranch: "feature/local-copy", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	attach := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "attach", LocalBranch: "original-local-name", WorktreeID: strings.Repeat("2", 32), Source: base, Target: base, CreatedAt: time.Now().UTC()}
	position := attach
	position.ID, position.Kind = strings.Repeat("3", 32), "position"
	position.LocalBranch = pr.HeadBranch // local alias renames keep the same context identity
	position.Source, position.Target, position.GitAfter = source, source, pr.HeadSHA
	position.CreatedAt = attach.CreatedAt.Add(time.Second)
	for _, e := range []domain.HistoryEvent{attach, position} {
		if err := svc.RecordHistory(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != source {
		t.Fatalf("promoted target: %+v %v", ref, err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, pr.HeadBranch); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alias became a separate context branch: %v", err)
	}
	rows, err := svc.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	bindings, completions := 0, 0
	for _, e := range rows {
		if e.Kind != "pr-merge" {
			continue
		}
		if e.Source != source || e.SourceBranchID != attach.BranchID {
			t.Fatalf("lost canonical source binding: %+v", e)
		}
		if e.PRCompleted {
			completions++
		} else {
			bindings++
		}
	}
	if bindings != 1 || completions != 1 {
		t.Fatalf("bindings=%d completions=%d", bindings, completions)
	}
}

func TestPRTrackingAliasRejectsUnprovenOrAmbiguousAssociations(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("alias-resolution")
	st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"})
	source := prSnapshot(t, st, repo, "source")
	pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "local-alias", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	attach := domain.HistoryEvent{Kind: "attach", Branch: "team/task", BranchID: "task", LocalBranch: pr.HeadBranch, WorktreeID: strings.Repeat("1", 32), CreatedAt: time.Now().UTC()}
	position := attach
	position.Kind, position.Target, position.GitAfter = "position", source, pr.HeadSHA
	position.CreatedAt = attach.CreatedAt.Add(time.Second)
	for _, tc := range []struct {
		name string
		edit func(*domain.HistoryEvent)
	}{
		{"wrong Git head", func(e *domain.HistoryEvent) { e.GitAfter = strings.Repeat("c", 40) }},
		{"different local name", func(e *domain.HistoryEvent) { e.LocalBranch = "unrelated" }},
		{"different worktree", func(e *domain.HistoryEvent) { e.WorktreeID = strings.Repeat("2", 32) }},
		{"missing worktree", func(e *domain.HistoryEvent) { e.WorktreeID = "" }},
		{"different identity", func(e *domain.HistoryEvent) { e.BranchID = "other-task" }},
		{"observation before attachment", func(e *domain.HistoryEvent) { e.CreatedAt = attach.CreatedAt.Add(-time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := position
			tc.edit(&changed)
			if _, _, err := svc.resolvePRSource(ctx, repo, pr, []domain.HistoryEvent{attach, changed}); err == nil {
				t.Fatal("unproven source accepted")
			}
		})
	}
	if _, _, err := svc.resolvePRSource(ctx, repo, pr, []domain.HistoryEvent{position}); err == nil {
		t.Fatal("unattached alias accepted")
	}
	otherAttach, otherPosition := attach, position
	otherAttach.BranchID, otherPosition.BranchID = "other-task", "other-task"
	otherAttach.WorktreeID, otherPosition.WorktreeID = strings.Repeat("2", 32), strings.Repeat("2", 32)
	if _, _, err := svc.resolvePRSource(ctx, repo, pr, []domain.HistoryEvent{attach, position, otherAttach, otherPosition}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ambiguous source identity accepted: %v", err)
	}
}
