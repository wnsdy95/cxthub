package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"testing"
	"time"
)

type scanReader struct {
	headPages map[int][]outbound.GitHead
	headReads []int
	headError bool
	deltas    map[string]domain.GitCommitDelta
	offline   bool
	reads     int
}

func (f *scanReader) ReadCommitDeltas(_ context.Context, _, sha string) ([]domain.GitCommitDelta, error) {
	f.reads++
	if f.offline {
		return nil, errors.New("offline")
	}
	d, ok := f.deltas[sha]
	if !ok {
		return nil, fmt.Errorf("unexpected commit %s", sha)
	}
	return []domain.GitCommitDelta{d}, nil
}
func (f *scanReader) ReadCommitTree(_ context.Context, _, sha string) (domain.GitTreeEvidence, error) {
	if f.offline {
		return domain.GitTreeEvidence{}, errors.New("offline")
	}
	d, ok := f.deltas[sha]
	if !ok {
		return domain.GitTreeEvidence{}, errors.New("unknown commit")
	}
	// Synthetic fixtures use flat filenames. Apply their explicit linear history.
	entries := map[string]domain.GitEntry{}
	var walk func(string)
	walk = func(id string) {
		v := f.deltas[id]
		if len(v.Parents) > 0 {
			walk(v.Parents[0])
		}
		for _, c := range v.Changes {
			if c.After.OID == "" {
				delete(entries, c.Path)
			} else {
				entries[c.Path] = c.After
			}
		}
	}
	walk(sha)
	n, e := domain.NewGitTreeNode(entries, 40)
	if e != nil {
		return domain.GitTreeEvidence{}, e
	}
	parents := append([]string{}, d.Parents...)
	return domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: sha, Tree: n.OID, Parents: parents}, Nodes: []domain.GitTreeNode{n}}, nil
}
func (f *scanReader) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	f.headReads = append(f.headReads, page)
	if f.headError {
		return nil, false, errors.New("offline")
	}
	heads := f.headPages[page]
	_, more := f.headPages[page+1]
	return heads, more, nil
}
func (f *scanReader) ReadCommitDelta(_ context.Context, _, sha, parent string) (domain.GitCommitDelta, error) {
	return domain.GitCommitDelta{}, fmt.Errorf("immutable indexed delta was fetched again: %s", sha)
}
func (f *scanReader) IsGitAncestor(_ context.Context, _, a, b string) (bool, error) {
	seen := map[string]bool{}
	todo := []string{b}
	for len(todo) > 0 {
		x := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if x == a {
			return true, nil
		}
		if seen[x] {
			continue
		}
		seen[x] = true
		todo = append(todo, f.deltas[x].Parents...)
	}
	return false, nil
}
func TestGitScansDiscoverUnlabelledReversalAndReapplication(t *testing.T) {
	core, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	origin := "https://github.com/example/project"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	oid := func(c string) string { return strings.Repeat(c, 40) }
	root, a, b, revert, reapply := oid("1"), oid("2"), oid("3"), oid("4"), oid("5")
	old := domain.GitEntry{OID: oid("a"), Mode: "100644"}
	fresh := domain.GitEntry{OID: oid("b"), Mode: "100755"}
	feature := domain.GitPathChange{Path: "feature", Before: old, After: fresh}
	inverse := domain.GitPathChange{Path: "feature", Before: fresh, After: old}
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{
		root:    {Commit: root, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true},
		a:       {Commit: a, Parents: []string{root}, Parent: root, Changes: []domain.GitPathChange{feature}, Complete: true},
		b:       {Commit: b, Parents: []string{a}, Parent: a, Changes: []domain.GitPathChange{{Path: "unrelated", After: old}}, Complete: true},
		revert:  {Commit: revert, Parents: []string{b}, Parent: b, Changes: []domain.GitPathChange{inverse}, Complete: true},
		reapply: {Commit: reapply, Parents: []string{revert}, Parent: revert, Changes: []domain.GitPathChange{feature}, Complete: true},
	}}
	scans, _ := NewGitScans(core, reader)
	// There are no commit messages, CLI-specific hints, or manual target pairs.
	// Observe only the newest tip: the candidate arrives before its target.
	for i := 0; i < 2; i++ {
		n, err := scans.ObservePush(ctx, origin, "refs/heads/main", "", reapply, false, "delivery-1")
		if err != nil || n != 1 {
			t.Fatal(n, err)
		}
	}
	if reader.reads != 0 {
		t.Fatal("provider I/O occurred during acceptance")
	}
	for i := 0; i < 20; i++ {
		if err := scans.Process(ctx, 1); err != nil {
			t.Fatal(err)
		}
		scans, _ = NewGitScans(core, reader)
	}
	jobs, err := scans.ListScans(ctx, repo, "", 100)
	if err != nil || len(jobs.Items) != 5 {
		t.Fatalf("scan closure %d %v", len(jobs.Items), err)
	}
	for _, j := range jobs.Items {
		if j.State != "completed" {
			t.Fatalf("unfinished %+v", j)
		}
	}
	if reader.reads != 5 {
		t.Fatal("duplicate provider reads", reader.reads)
	}
	changes, _ := NewGitChanges(core, reader)
	if err = changes.Process(ctx, 100); err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	page, err := changes.List(ctx, repo, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range page.Items {
		if j.State != "completed" {
			t.Fatalf("verification failed %+v", j)
		}
		if j.Coverage == "full" {
			found[j.Request.Target] = j.Request.Commit
		}
	}
	if found[a] != revert || found[revert] != reapply {
		t.Fatalf("missing automatic proof %+v", found)
	}
	if _, ok := found[b]; ok {
		t.Fatal("unrelated B was reversed")
	}
	history, _ := core.ListHistory(ctx, repo)
	refs, _ := core.ListRefs(ctx, repo)
	if len(history) != 0 || len(refs) != 0 {
		t.Fatal("discovery moved history/refs")
	}
	// A late old observation preserves newer proof and only reuses cached work.
	if _, err = scans.ObservePush(ctx, origin, "refs/heads/main", reapply, a, true, "delivery-2"); err != nil {
		t.Fatal(err)
	}
	if err = scans.Process(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if reader.reads != 5 {
		t.Fatal("out-of-order delivery repeated reads")
	}
}
func TestGitScanOfflineRetryAndOriginFence(t *testing.T) {
	core, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	origin := "https://github.com/example/project"
	sha := strings.Repeat("a", 40)
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	reader := &scanReader{offline: true, deltas: map[string]domain.GitCommitDelta{sha: {Commit: sha, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}}}
	scans, _ := NewGitScans(core, reader)
	if _, err := scans.ObservePush(ctx, origin, "refs/heads/main", "", sha, false, "delivery-3"); err != nil {
		t.Fatal(err)
	}
	if err := scans.Process(ctx, 1); err != nil {
		t.Fatal(err)
	}
	j, err := st.GetGitScan(ctx, repo, domain.NewGitScan(repo, origin, sha, time.Now()).ID)
	if err != nil || j.State != "retrying" || j.Indexed {
		t.Fatal(j, err)
	}
	reader.offline = false
	if err = scans.RetryScan(ctx, repo, j.ID); err != nil {
		t.Fatal(err)
	}
	if err = scans.Process(ctx, 3); err != nil {
		t.Fatal(err)
	}
	j, _ = st.GetGitScan(ctx, repo, j.ID)
	if j.State != "completed" {
		t.Fatal(j)
	}
	// Bound origin cannot be silently swapped between read and publication.
	other := strings.Repeat("b", 40)
	scans.ObservePush(ctx, origin, "refs/heads/main", sha, other, false, "delivery-4")
	core.meta = reboundGitRepo{MetadataStore: core.meta}
	claimed, err := st.ClaimGitScan(ctx, repo, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = scans.run(ctx, claimed); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
}
func TestGitScanRejectsPartialParentEvidence(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 40) }
	j := domain.NewGitScan(hh(t.Name()), "https://github.com/example/project", oid("1"), time.Now())
	d := domain.GitCommitDelta{Commit: j.Commit, Parents: []string{oid("2"), oid("3")}, Parent: oid("2"), Changes: []domain.GitPathChange{}, Complete: true}
	p := domain.GitScanFinish{Job: j}
	if err := planGitIndex(&p, []domain.GitCommitDelta{d}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("missing merge comparison accepted", err)
	}
	d.Parents = []string{oid("2")}
	d.Complete = false
	p = domain.GitScanFinish{Job: j}
	if err := planGitIndex(&p, []domain.GitCommitDelta{d}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("partial tree accepted", err)
	}
}

func TestAcceptedHistoryQueuesWithoutChangingWorktreeSelection(t *testing.T) {
	core, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	origin := "https://github.com/example/project"
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	sha := strings.Repeat("a", 40)
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "local-task", Branch: "feature", Kind: "birth", GitAfter: sha, CreatedAt: time.Now().UTC()}
	for i := 0; i < 2; i++ {
		if err := core.RecordHistory(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := st.ListGitScans(ctx, repo, "", 100)
	if err != nil || len(jobs) != 1 || jobs[0].Commit != sha {
		t.Fatal(jobs, err)
	}
	history, _ := core.ListHistory(ctx, repo)
	if len(history) != 1 || history[0].ID != e.ID {
		t.Fatal("queue reinterpreted source history")
	}
}

func TestGitHeadReconciliationResumesPagesAndPublishesFailures(t *testing.T) {
	core, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh(t.Name())
	origin := "https://github.com/example/project"
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	reader := &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/a", Commit: strings.Repeat("a", 40)}}, 2: {{Ref: "refs/heads/b", Commit: strings.Repeat("b", 40)}}}}
	g, _ := NewGitScans(core, reader)
	if err := g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Restart application composition after page one. The cursor is store-owned.
	g, _ = NewGitScans(core, reader)
	if err := g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := g.ListScans(ctx, repo, "", 100)
	if err != nil || len(page.Items) != 2 || page.Reconciliation == nil || page.Reconciliation.State != "completed" {
		t.Fatal(page, err)
	}
	if len(reader.headReads) != 2 || reader.headReads[0] != 1 || reader.headReads[1] != 2 {
		t.Fatal("page replayed", reader.headReads)
	}
	if err = g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reader.headReads) != 2 {
		t.Fatal("reconciler busy-polled provider")
	}
	other := hh(t.Name() + "offline")
	st.PutRepo(ctx, domain.Repo{ID: other, GitRemoteURL: origin})
	reader.headError = true
	if err = g.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = g.ListScans(ctx, other, "", 100)
	if err != nil || page.Reconciliation == nil || page.Reconciliation.State != "retrying" || page.Reconciliation.Page != 1 {
		t.Fatal("failed discovery hidden", page, err)
	}
}
