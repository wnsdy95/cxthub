package domain

import (
	"testing"
	"time"
)

func TestPRPublicationAcceptsSharedAliasAcrossWorktrees(t *testing.T) {
	at := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	publication := HistoryEvent{Kind: "publish", RepoID: "repo", Branch: "main", BranchID: "main-id", LocalBranch: "feature/review", WorktreeID: "worker", GitAfter: "exact-head", Target: HashContent([]byte("captured context")), CreatedAt: at.Add(time.Hour)}
	proof := publication
	proof.Kind = "position"
	proof.CreatedAt = at.Add(time.Minute)
	attached := HistoryEvent{Kind: "attach", RepoID: publication.RepoID, Branch: "main", BranchID: publication.BranchID, LocalBranch: publication.LocalBranch, WorktreeID: "creator", CreatedAt: at}
	tests := []struct {
		name string
		edit func(*HistoryEvent, *HistoryEvent)
		want bool
	}{
		{"same alias another worktree", func(*HistoryEvent, *HistoryEvent) {}, true},
		{"unrelated alias another worktree", func(a, p *HistoryEvent) { a.LocalBranch = "different" }, false},
		{"same worktree alias renamed", func(a, p *HistoryEvent) { a.WorktreeID = publication.WorktreeID; a.LocalBranch = "old-name" }, true},
		{"attachment without worktree evidence", func(a, p *HistoryEvent) { a.WorktreeID = "" }, false},
		{"different identity", func(a, p *HistoryEvent) { a.BranchID = "other-id" }, false},
		{"different repository", func(a, p *HistoryEvent) { a.RepoID = "other-repo" }, false},
		{"attachment after proof", func(a, p *HistoryEvent) { a.CreatedAt = at.Add(2 * time.Minute) }, false},
		{"proof for a different head", func(a, p *HistoryEvent) { p.GitAfter = "other-head" }, false},
		{"proof for a different worktree", func(a, p *HistoryEvent) { p.WorktreeID = "other-worker" }, false},
		{"proof for a different context", func(a, p *HistoryEvent) { p.Target = HashContent([]byte("different context")) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, p := attached, proof
			tt.edit(&a, &p)
			if got := MatchesPRSourcePublication(publication, publication.LocalBranch, []HistoryEvent{a, p}); got != tt.want {
				t.Fatalf("matches=%v, want %v", got, tt.want)
			}
		})
	}
}
