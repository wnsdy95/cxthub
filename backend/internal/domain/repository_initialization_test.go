package domain

import (
	"strings"
	"testing"
)

func TestRepositoryInitializationAnchorValidation(t *testing.T) {
	repo, target := HashContent([]byte("repo")), HashContent([]byte("target"))
	good := func() RepositoryInitializationAnchor {
		return RepositoryInitializationAnchor{
			Ref:            Ref{RepoID: repo, Kind: RefBranch, Name: "main", Target: target, BranchID: LegacyContextBranchID(string(repo), "main")},
			SnapshotStates: map[ContentHash]ContentHash{target: HashContent([]byte("state"))}}
	}
	if err := good().Validate(repo); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*RepositoryInitializationAnchor)
	}{
		{"foreign_repo", func(a *RepositoryInitializationAnchor) { a.Ref.RepoID = HashContent([]byte("foreign")) }},
		{"modern_birth_not_legacy", func(a *RepositoryInitializationAnchor) { a.Ref.BranchID = "genuine-birth" }},
		{"identity_newline", func(a *RepositoryInitializationAnchor) { a.Ref.BranchID += "\n" }},
		{"branch_newline", func(a *RepositoryInitializationAnchor) { a.Ref.Name += "\n" }},
		{"symbolic", func(a *RepositoryInitializationAnchor) { a.Ref.Symbolic = "other" }},
		{"tag", func(a *RepositoryInitializationAnchor) { a.Ref.Kind = RefTag }},
		{"missing_target", func(a *RepositoryInitializationAnchor) { a.SnapshotStates = map[ContentHash]ContentHash{} }},
		{"invalid_state", func(a *RepositoryInitializationAnchor) { a.SnapshotStates[target] = "not-a-hash" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := good()
			tc.change(&a)
			if a.Validate(repo) == nil {
				t.Fatal("unsafe anchor accepted")
			}
		})
	}
	a, b := good(), good()
	if !a.Equal(b) {
		t.Fatal("equal payload rejected")
	}
	b.SnapshotStates[target] = HashContent([]byte("new state"))
	if a.Equal(b) {
		t.Fatal("same ID changed payload accepted")
	}
	for _, id := range []string{"", "init_" + strings.Repeat("A", 32), "init_" + strings.Repeat("a", 31), "init_" + strings.Repeat("z", 32)} {
		if ValidateRepositoryInitializationID(id) == nil {
			t.Fatal("invalid creation ID accepted")
		}
	}
	if ValidateRepositoryInitializationID("init_"+strings.Repeat("a", 32)) != nil {
		t.Fatal("valid ID rejected")
	}
}

func TestRepositoryInitializationBranchEligibility(t *testing.T) {
	repo := HashContent([]byte("repo"))
	legacy := LegacyContextBranchID(string(repo), "main")
	for _, tc := range []struct {
		name    string
		event   HistoryEvent
		allowed bool
	}{
		{"ordinary", HistoryEvent{Kind: "position", Branch: "main", BranchID: legacy}, true},
		{"publish", HistoryEvent{Kind: "publish", Branch: "main", BranchID: legacy}, true},
		{"unrelated_birth", HistoryEvent{Kind: "birth", Branch: "feature", BranchID: "feature"}, true},
		{"legacy_origin", HistoryEvent{Kind: "birth", Branch: "feature", BranchID: "feature", Creation: &GitCreation{OriginBranch: "main", OriginBranchID: legacy}}, true},
		{"conflicting_origin", HistoryEvent{Kind: "birth", Branch: "feature", BranchID: "feature", Creation: &GitCreation{OriginBranch: "main", OriginBranchID: "other"}}, false},
		{"foreign_pin", HistoryEvent{Kind: "position", Branch: "main", BranchID: "other"}, false},
		{"renamed", HistoryEvent{Kind: "rename", Branch: "other", PreviousBranch: "main", BranchID: legacy}, false},
		{"identity_elsewhere", HistoryEvent{Kind: "archive", Branch: "elsewhere", BranchID: legacy}, false},
		{"pr_head_conflict", HistoryEvent{Kind: "pr-merge", Branch: "base", BranchID: "base", SourceBranchID: "other", PR: &PullRequestMerge{HeadBranch: "main", BaseBranch: "base"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRepositoryInitializationBranch(repo, "main", nil, []HistoryEvent{tc.event}, nil)
			if (err == nil) != tc.allowed {
				t.Fatal(err)
			}
		})
	}
}
