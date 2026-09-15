package domain

import (
	"strings"
	"testing"
	"time"
)

func TestContextBranchIdentityFollowsCausalRenameAndReuse(t *testing.T) {
	repo := string(HashContent([]byte("repo")))
	birth := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "first", Branch: "feature/a", Kind: "birth", CreatedAt: time.Now().UTC()}
	rename := HistoryEvent{ID: strings.Repeat("2", 32), RepoID: repo, BranchID: "first", Branch: "feature/b", PreviousBranch: "feature/a", BindingParent: birth.ID, Kind: "rename", CreatedAt: birth.CreatedAt.Add(-time.Hour)}
	reuse := HistoryEvent{ID: strings.Repeat("3", 32), RepoID: repo, BranchID: "second", Branch: "feature/a", BindingParent: rename.ID, Kind: "birth", CreatedAt: birth.CreatedAt.Add(-2 * time.Hour)}
	// Clock order and input order do not establish identity; the recorded
	// dependency does. Both names may point to exactly the same conversation.
	state, err := ProjectContextBranches([]HistoryEvent{reuse, rename, birth})
	if err != nil {
		t.Fatal(err)
	}
	if state.Active["feature/b"].ID != "first" || state.Active["feature/a"].ID != "second" {
		t.Fatalf("identity lost: %+v", state)
	}
	conflict := reuse
	conflict.ID, conflict.BranchID = strings.Repeat("4", 32), "third"
	if _, err := ProjectContextBranches([]HistoryEvent{birth, rename, reuse, conflict}); err == nil {
		t.Fatal("independent same-name births merged")
	}
	rename.BindingParent = reuse.ID
	if _, err := ProjectContextBranches([]HistoryEvent{birth, rename, reuse}); err == nil {
		t.Fatal("cyclic/mismatched dependencies accepted")
	}
}

func TestLegacyContextBranchCanRenameWithoutInventingBirth(t *testing.T) {
	repo := string(HashContent([]byte("repo")))
	e := HistoryEvent{ID: strings.Repeat("5", 32), RepoID: repo, BranchID: LegacyContextBranchID(repo, "old"), Branch: "new", PreviousBranch: "old", Kind: "rename", CreatedAt: time.Now().UTC()}
	state, err := ProjectContextBranches([]HistoryEvent{e})
	if err != nil {
		t.Fatal(err)
	}
	if state.Active["new"].ID != e.BranchID || state.Released["old"] != e.ID {
		t.Fatalf("legacy rename: %+v", state)
	}
	e.BranchID = "unrelated"
	if _, err := ProjectContextBranches([]HistoryEvent{e}); err == nil {
		t.Fatal("unknown identity claimed a legacy branch")
	}
}
