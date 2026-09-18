package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type treeTestStore interface {
	outbound.GitTreeStore
	outbound.GitScanStore
	outbound.MetadataStore
}

func checkGitTreeUpgrade(t *testing.T, st treeTestStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	repo := domain.HashContent([]byte(t.Name() + now.String()))
	origin := "https://github.com/example/tree-upgrade"
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	j := domain.NewGitScan(repo, origin, strings.Repeat("c", 40), now)
	st.EnqueueGitScan(ctx, j)
	j, e := st.ClaimGitScan(ctx, repo, now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	// Simulate a completed pre-upgrade job, preserving its historical discovery.
	j.State = "completed"
	j.Indexed = true
	d := domain.GitCommitDelta{Commit: j.Commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}
	if e = st.FinishGitScan(ctx, domain.GitScanFinish{Job: j, Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(repo, origin, d)}}); e != nil {
		t.Fatal(e)
	}
	first, e := st.ClaimGitScan(ctx, repo, now, time.Minute)
	if e != nil || !first.Indexed {
		t.Fatal("older completion was not upgradeable", e)
	}
	second, e := st.ClaimGitScan(ctx, repo, now.Add(2*time.Minute), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	n, _ := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	proof := domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: j.Commit, Tree: n.OID, Parents: []string{}}, Nodes: []domain.GitTreeNode{n}}
	p := domain.GitScanFinish{Job: first, Tree: &proof}
	p.Job.TreeIndexed = true
	p.Job.State = "completed"
	if e = st.FinishGitScan(ctx, p); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("expired worker published tree", e)
	}
	if _, e = st.GetGitCommitTree(ctx, repo, origin, j.Commit); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("stale binding leaked", e)
	}
	p.Job = second
	p.Job.TreeIndexed = true
	p.Job.State = "completed"
	broken := p
	bad := proof
	bad.Nodes = nil
	broken.Tree = &bad
	if e = st.FinishGitScan(ctx, broken); !errors.Is(e, domain.ErrIntegrity) {
		t.Fatal("partial tree accepted", e)
	}
	if e = st.FinishGitScan(ctx, p); e != nil {
		t.Fatal(e)
	}
	got, e := st.GetGitCommitTree(ctx, repo, origin, j.Commit)
	if e != nil || got.Tree != n.OID {
		t.Fatal(got, e)
	}
	if _, e = st.GetGitTreeNode(ctx, repo, origin, n.OID); e != nil {
		t.Fatal(e)
	}
	if _, e = st.GetGitTreeNode(ctx, repo, origin+"-other", n.OID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("origin leak", e)
	}
	if _, e = st.GetGitCommitTree(ctx, domain.HashContent([]byte("other")), origin, j.Commit); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("tenant leak", e)
	}
	if _, e = st.ClaimGitScan(ctx, repo, now.Add(time.Hour), time.Minute); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("completed tree requeued", e)
	}
}
func TestFSGitTreeUpgrade(t *testing.T) { checkGitTreeUpgrade(t, NewFSStore(t.TempDir())) }
