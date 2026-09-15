package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"sync"
	"testing"
	"time"
)

func prSnapshot(t *testing.T, st *store.FSStore, repo domain.ContentHash, text string, parents ...domain.ContentHash) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion = "1"
	doc.CIR.Envelope.SourceProvider = "claude"
	doc.CIR.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "feature/x", Provider: "claude", Parents: parents}); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

func TestPRBindingSurvivesRenameReuseAndConcurrentReplay(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("pr-repo")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "git@github.com:acme/proj.git"}); err != nil {
		t.Fatal(err)
	}
	m := prSnapshot(t, st, repo, "main")
	f := prSnapshot(t, st, repo, "original PR", m)
	other := prSnapshot(t, st, repo, "new task", m)
	for name, target := range map[string]domain.ContentHash{"main": m, "feature/x": f} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: name, Target: target}, ""); err != nil {
			t.Fatal(err)
		}
	}
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "original", Branch: pr.HeadBranch, Kind: "birth", Source: m, Target: f, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, birth); err != nil {
		t.Fatal(err)
	}
	renamed := birth
	renamed.ID = strings.Repeat("2", 32)
	renamed.Kind = "rename"
	renamed.Branch = "feature/renamed"
	renamed.PreviousBranch = birth.Branch
	renamed.BindingParent = birth.ID
	renamed.Source = f
	if err := st.ApplyHistoryEvent(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	reused := birth
	reused.ID = strings.Repeat("3", 32)
	reused.BranchID = "new-task"
	reused.BindingParent = renamed.ID
	reused.Target = other
	reused.GitAfter = strings.Repeat("c", 40)
	if err := st.ApplyHistoryEvent(ctx, reused); err != nil {
		t.Fatal(err)
	}
	n, err := svc.PromoteMergedPR(ctx, "https://github.com/acme/proj.git", pr)
	if err != nil || n != 1 {
		t.Fatalf("promote=%d %v", n, err)
	}
	current, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || current.Target != f {
		t.Fatalf("wrong source: %+v %v", current, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); err != nil {
				t.Errorf("retry: %v", err)
			}
		}()
	}
	wg.Wait()
	events, err := svc.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == "pr-merge" {
			count++
			if e.Source != f || e.SourceBranchID != "original" {
				t.Fatalf("bad receipt: %+v", e)
			}
			if err := svc.RecordHistory(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("receipts=%d", count)
	}
	pr.HeadSHA = strings.Repeat("c", 40)
	if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("rewritten PR accepted: %v", err)
	}
}

func TestPRBindingRejectsAmbiguousSourceAndUntrustedReceipt(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("ambiguous-pr")
	st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/a/b"})
	m := prSnapshot(t, st, repo, "main")
	a := prSnapshot(t, st, repo, "a", m)
	b := prSnapshot(t, st, repo, "b", m)
	st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: m}, "")
	pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	for i, target := range []domain.ContentHash{a, b} {
		e := domain.HistoryEvent{ID: strings.Repeat(string(rune('1'+i)), 32), RepoID: string(repo), BranchID: "work", Branch: pr.HeadBranch, Kind: "position", Target: target, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
		if err := st.ApplyHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.PromoteRepositoryPR(ctx, repo, pr); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ambiguous source: %v", err)
	}
	events, _ := svc.ListHistory(ctx, repo)
	if len(events) != 2 {
		t.Fatal("failed resolution published receipt")
	}
	forged := domain.HistoryEvent{ID: strings.Repeat("9", 32), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "pr-merge", Source: a, Target: a, SourceBranchID: "work", PR: &pr, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(ctx, forged); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("forged receipt accepted: %v", err)
	}
	ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if ref.Target != m {
		t.Fatal("base changed")
	}
}

func TestPRConcurrentFirstDeliveryFreezesExactCommit(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("pr-first-race")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/race"}); err != nil {
		t.Fatal(err)
	}
	m := prSnapshot(t, st, repo, "base")
	exact := prSnapshot(t, st, repo, "merged commit", m)
	newer := prSnapshot(t, st, repo, "work after the merged commit", exact)
	for name, target := range map[string]domain.ContentHash{"main": m, "feature/x": newer} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: name, Target: target}, ""); err != nil {
			t.Fatal(err)
		}
	}
	pr := domain.PullRequestMerge{Number: 13, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	e := domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(repo), BranchID: "feature-task", Branch: "feature/x", Kind: "birth", Source: m, Target: m, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(ctx, e); err != nil {
		t.Fatal(err)
	}
	pos := domain.HistoryEvent{ID: strings.Repeat("4", 32), RepoID: string(repo), BranchID: e.BranchID, Branch: e.Branch, Kind: "position", Source: exact, Target: exact, GitAfter: pr.HeadSHA, CreatedAt: e.CreatedAt.Add(time.Second)}
	if err := svc.RecordHistory(ctx, pos); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { <-start; _, err := svc.PromoteRepositoryPR(ctx, repo, pr); results <- err }()
	}
	close(start)
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != exact {
		t.Fatalf("promoted newer unrelated tip: %+v %v", ref, err)
	}
	rows, err := svc.ListHistory(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.Kind == "pr-merge" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("receipts=%d", count)
	}
	pr.Number++
	pr.HeadSHA = strings.Repeat("c", 40)
	if _, err := svc.PromoteMergedPR(ctx, "https://github.com/acme/race", pr); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("missing association silently acknowledged: %v", err)
	}
}
