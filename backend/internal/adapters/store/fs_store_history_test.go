package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestConcurrentContextBirthsCannotShareOneActiveName(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := rlHash('0')
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			results <- s.ApplyHistoryEvent(ctx, domain.HistoryEvent{ID: id, BranchID: id, Branch: "same-name", Kind: "birth", RepoID: string(repo), CreatedAt: time.Now().UTC()})
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, domain.ErrRefConflict) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	events, err := s.ListHistoryEvents(ctx, repo)
	if err != nil || len(events) != 1 {
		t.Fatalf("competing identity published: %+v %v", events, err)
	}
}

func TestHistoryAdvanceRetainsSourceAndReplayDoesNotRewindAgain(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := rlHash('0')
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	a, b, c := rlHash('a'), rlHash('b'), rlHash('c')
	for _, id := range []domain.ContentHash{a, b, c} {
		if err := s.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: id, DocHash: id, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: a}, ""); err != nil {
		t.Fatal(err)
	}
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "test", Branch: "main", Kind: "advance", Source: a, Target: b, CreatedAt: time.Now().UTC()}
	if err := s.ApplyHistoryEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	root, err := s.GetRef(ctx, repo, domain.RefTag, "cxt/history/v1/"+e.ID+"/source")
	if err != nil || root.Target != a {
		t.Fatalf("source not retained: %+v %v", root, err)
	}
	if err := s.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: c}, b); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHistoryEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	head, _ := s.GetRef(ctx, repo, domain.RefBranch, "main")
	if head.Target != c {
		t.Fatal("replay rewound newer work")
	}
	e.Target = c
	if err := s.ApplyHistoryEvent(ctx, e); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("ID mutation accepted: %v", err)
	}
	e.ID = strings.Repeat("2", 32)
	if err := s.ApplyHistoryEvent(ctx, e); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("stale source accepted: %v", err)
	}
	events, err := s.ListHistoryEvents(ctx, repo)
	if err != nil || len(events) != 1 {
		t.Fatalf("history=%+v %v", events, err)
	}
	protected := true
	if err := s.UpdateRepoConfig(ctx, repo, nil, &protected); err != nil {
		t.Fatal(err)
	}
	// Policy changes do not revoke an accepted immutable receipt, but new
	// history publication cannot bypass the ordinary protected-branch policy.
	if err := s.ApplyHistoryEvent(ctx, events[0]); err != nil {
		t.Fatalf("acknowledged retry after protection: %v", err)
	}
	e.ID, e.Source, e.Target = strings.Repeat("4", 32), c, b
	if err := s.ApplyHistoryEvent(ctx, e); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("history bypassed branch protection: %v", err)
	}
	head, err = s.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || head.Target != c {
		t.Fatalf("rejected advance changed branch: %+v %v", head, err)
	}
}

func TestHistoryRejectsWrongIdentityEvenWhenSourceHashMatches(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	repo := rlHash('0')
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	a, b := rlHash('a'), rlHash('b')
	for _, id := range []domain.ContentHash{a, b} {
		if err := s.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
			t.Fatal(err)
		}
	}
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "new-task", Branch: "reused", Kind: "birth", Source: a, Target: a, CreatedAt: time.Now().UTC()}
	if err := s.ApplyHistoryEvent(ctx, birth); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: birth.Branch, Target: a}, ""); err != nil {
		t.Fatal(err)
	}
	stale := birth
	stale.ID, stale.BranchID, stale.Kind, stale.Target = strings.Repeat("2", 32), "old-task", "advance", b
	if err := s.ApplyHistoryEvent(ctx, stale); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("wrong identity accepted: %v", err)
	}
	head, _ := s.GetRef(ctx, repo, domain.RefBranch, birth.Branch)
	if head.Target != a {
		t.Fatal("wrong identity advanced branch")
	}
	stale.BranchID = birth.BranchID
	if err := s.ApplyHistoryEvent(ctx, stale); err != nil {
		t.Fatal(err)
	}
	rename := birth
	rename.ID, rename.Kind, rename.Branch, rename.PreviousBranch, rename.BindingParent = strings.Repeat("3", 32), "rename", "new-name", birth.Branch, birth.ID
	if err := s.ApplyHistoryEvent(ctx, rename); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("stale rename accepted after continuation: %v", err)
	}
}

func TestHistoryJournalCompletesPublicationBeforeNextRefMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewFSStore(dir)
	repo := rlHash('0')
	a, b, c := rlHash('a'), rlHash('b'), rlHash('c')
	for _, id := range []domain.ContentHash{a, b, c} {
		if err := s.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: id, DocHash: id, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: a}, ""); err != nil {
		t.Fatal(err)
	}
	e := domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(repo), BranchID: "test", Branch: "main", Kind: "advance", Source: a, Target: b, CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(e)
	if err := writeAtomic(s.historyJournal(repo), raw); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption before publishing retention/ref; next mutation must
	// complete A -> B first, making this stale A -> C CAS fail.
	if err := s.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: c}, a); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("interrupted publication bypassed: %v", err)
	}
	if _, err := os.Stat(s.historyJournal(repo)); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
	reopened, err := OpenFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := reopened.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || head.Target != b {
		t.Fatalf("head=%+v %v", head, err)
	}
	events, err := reopened.ListHistoryEvents(ctx, repo)
	if err != nil || len(events) != 1 {
		t.Fatalf("history=%+v %v", events, err)
	}
}
