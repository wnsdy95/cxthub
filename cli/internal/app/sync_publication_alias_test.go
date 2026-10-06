package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestHistoryPushCrossWorktreeAliasPreflight(t *testing.T) {
	for _, maximalHasAlias := range []bool{false, true} {
		name := "maximal misses shared alias"
		if maximalHasAlias {
			name = "maximal covers shared alias"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			a := publicationSnapshot(t, st, repo, "A", nil, nil)
			b := publicationSnapshot(t, st, repo, "B", []domain.ContentHash{a}, nil)
			p := domain.HistoryEvent{RepoID: repo, BranchID: "task", Branch: "team/task", LocalBranch: "local-task", WorktreeID: strings.Repeat("2", 32), Target: a, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
			attached := p
			attached.ID, attached.Kind = strings.Repeat("f", 32), "attach"
			attached.WorktreeID, attached.CreatedAt = strings.Repeat("1", 32), time.Unix(1, 0).UTC()
			if err := st.PutHistoryEvent(ctx, attached); err != nil {
				t.Fatal(err)
			}
			_, oa := putPublication(t, st, 1, p)
			p.Target, p.WorktreeID = b, strings.Repeat("3", 32)
			if !maximalHasAlias {
				p.LocalBranch = ""
			}
			_, ob := putPublication(t, st, 2, p)
			r := &publicationRemote{requiredOrdinary: []string{oa.ID, ob.ID}}
			err := newTestSyncService(st, r, nil).pushHistory(ctx, repo)
			alias := [3]string{repo, attached.LocalBranch, p.GitAfter}
			if !maximalHasAlias {
				if !errors.Is(err, domain.ErrSyncConflict) || len(r.accepted) != 0 || len(r.terminal) != 0 {
					t.Fatalf("ambiguous group escaped preflight: err=%v accepted=%d publications=%v aliasBound=%s ancestor=%s", err, len(r.accepted), publicationTargets(r), r.terminal[alias], a)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := publicationTargets(r); !reflect.DeepEqual(got, []domain.ContentHash{b, a}) || r.terminal[alias] != b {
				t.Fatalf("shared alias bound before maximal source: publications=%v aliasBound=%s want=%s", got, r.terminal[alias], b)
			}
		})
	}
}

func TestPublicationSourceNamesMatchBackendAliasEligibility(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	p := domain.HistoryEvent{Kind: "publish", RepoID: "repo", BranchID: "task", Branch: "team/task", LocalBranch: "local-task", WorktreeID: "capture-worktree", GitAfter: strings.Repeat("a", 40), Target: domain.HashContent([]byte("context")), CreatedAt: at.Add(time.Hour)}
	proof := p
	proof.Kind, proof.CreatedAt = "position", at.Add(time.Minute)
	attached := domain.HistoryEvent{Kind: "attach", RepoID: p.RepoID, Branch: p.Branch, BranchID: p.BranchID, LocalBranch: p.LocalBranch, WorktreeID: "creation-worktree", CreatedAt: at}
	// Expected values follow backend/domain/pr_publication_worktree_test.go.
	// No comparison to another copy of the implementation is used as an oracle.
	for _, tt := range []struct {
		name string
		edit func(*domain.HistoryEvent, *domain.HistoryEvent)
		want bool
	}{
		{"same alias another worktree", func(*domain.HistoryEvent, *domain.HistoryEvent) {}, true},
		{"unrelated alias another worktree", func(a, _ *domain.HistoryEvent) { a.LocalBranch = "other" }, false},
		{"same worktree renamed alias", func(a, _ *domain.HistoryEvent) { a.WorktreeID, a.LocalBranch = p.WorktreeID, "old-name" }, true},
		{"empty attachment worktree", func(a, _ *domain.HistoryEvent) { a.WorktreeID = "" }, false},
		{"different identity", func(a, _ *domain.HistoryEvent) { a.BranchID = "other" }, false},
		{"different repo", func(a, _ *domain.HistoryEvent) { a.RepoID = "other" }, false},
		{"late attachment", func(a, _ *domain.HistoryEvent) { a.CreatedAt = at.Add(2 * time.Minute) }, false},
		{"different proof revision", func(_, o *domain.HistoryEvent) { o.GitAfter = strings.Repeat("b", 40) }, false},
		{"different proof worktree", func(_, o *domain.HistoryEvent) { o.WorktreeID = "other" }, false},
		{"different proof context", func(_, o *domain.HistoryEvent) { o.Target = domain.HashContent([]byte("other")) }, false},
		{"ordinary attach proof", func(_, o *domain.HistoryEvent) { o.Kind = "attach" }, true},
		{"publication is not proof", func(_, o *domain.HistoryEvent) { o.Kind = "publish" }, false},
		{"PR receipt is not proof", func(_, o *domain.HistoryEvent) { o.Kind = "pr-merge" }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, o := attached, proof
			tt.edit(&a, &o)
			got := domain.PublicationSourceNames(p, []domain.HistoryEvent{a, o})
			if !slices.Contains(got, p.Branch) || slices.Contains(got, p.LocalBranch) != tt.want {
				t.Fatalf("source names=%v, want alias eligible=%v", got, tt.want)
			}
		})
	}
}
