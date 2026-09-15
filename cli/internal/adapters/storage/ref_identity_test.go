package storage

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestLegacyIdentityAdoptionKeepsLocalWorkAndHistoricalPosition(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte("identity migration"))
	st := NewWorktreeFileStore(t.TempDir(), "/fixture/.git", "main", strings.Repeat("a", 40))
	a := repairFixture(t, st, repo, "old position")
	b := repairFixture(t, st, repo, "local ahead", a)
	legacy := domain.LegacyContextBranchID(repo, "main")
	if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: legacy, Target: b}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", BranchID: legacy, GitCommit: strings.Repeat("a", 40), Snapshot: a, SharedTarget: b, MemoryPinned: true, Rewound: true}); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "cloud-first", Branch: "main", Kind: "birth", Source: a, Target: a, CreatedAt: time.Now().UTC()}
	if err := st.PutHistoryEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	remote := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: e.BranchID, Target: a}
	for i := 0; i < 2; i++ {
		if err := st.AdoptLegacyBranchIdentity(ctx, repo, remote); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != b || ref.BranchID != e.BranchID {
		t.Fatalf("pointer %+v %v", ref, err)
	}
	after, err := st.GetWorkingPosition(ctx)
	if err != nil || after.Snapshot != before.Snapshot || after.GitCommit != before.GitCommit || after.MemoryHash != before.MemoryHash || !after.Rewound || after.BranchID != e.BranchID {
		t.Fatalf("position %+v %v", after, err)
	}
	rows, err := st.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == before.Selection.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("old position observation lost")
	}
	if _, err := st.ListRefs(ctx, ""); err != nil {
		t.Fatal("unscoped local list cannot decode identity refs", err)
	}
}

func TestLegacyIdentityAdoptionNeverGuessesReusedName(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte("identity reuse"))
	st := NewFileStore(t.TempDir())
	a := repairFixture(t, st, repo, "unchanged snapshot")
	legacy := domain.LegacyContextBranchID(repo, "work")
	if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "work", BranchID: legacy, Target: a}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []domain.HistoryEvent{
		{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "old", Branch: "work", Kind: "birth", Source: a, Target: a, CreatedAt: now},
		{ID: strings.Repeat("b", 32), RepoID: repo, BranchID: "old", Branch: "renamed", PreviousBranch: "work", Kind: "rename", BindingParent: strings.Repeat("a", 32), Source: a, Target: a, CreatedAt: now.Add(time.Second)},
		{ID: strings.Repeat("c", 32), RepoID: repo, BranchID: "new", Branch: "work", Kind: "birth", BindingParent: strings.Repeat("b", 32), Source: a, Target: a, CreatedAt: now.Add(2 * time.Second)},
	}
	for _, e := range events {
		if err := st.PutHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AdoptLegacyBranchIdentity(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "work", BranchID: "new", Target: a}); err != nil {
		t.Fatal(err)
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "work")
	if err != nil || ref.BranchID != legacy {
		t.Fatalf("guessed same-hash identity %+v %v", ref, err)
	}
}
