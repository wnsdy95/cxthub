//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGitTreeUpgradeAndRollback(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, e := NewPostgresStore(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if _, e = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); e != nil {
		t.Fatal(e)
	}
	checkGitTreeUpgrade(t, st)
	now := time.Now().UTC()
	repo := domain.HashContent([]byte(t.Name() + now.String()))
	origin := "https://github.com/example/atomic-tree"
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	j := domain.NewGitScan(repo, origin, strings.Repeat("d", 40), now)
	st.EnqueueGitScan(ctx, j)
	j, e = st.ClaimGitScan(ctx, repo, now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	n, _ := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	proof := domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: j.Commit, Tree: n.OID, Parents: []string{}}, Nodes: []domain.GitTreeNode{n}}
	p := domain.GitScanFinish{Job: j, Tree: &proof}
	p.Job.State = "waiting"
	p.Job.TreeIndexed = true
	before, _ := st.RepositoryRevision(ctx, repo)
	sentinel := errors.New("abort after tree and revision")
	e = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if e := st.FinishGitScan(tx, p); e != nil {
			return e
		}
		if e := st.AdvanceEvidenceRevision(tx, repo); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	if _, e = st.GetGitCommitTree(ctx, repo, origin, j.Commit); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("binding survived rollback", e)
	}
	if _, e = st.GetGitTreeNode(ctx, repo, origin, n.OID); !errors.Is(e, domain.ErrNotFound) {
		t.Fatal("object survived rollback", e)
	}
	after, _ := st.RepositoryRevision(ctx, repo)
	if before != after {
		t.Fatal("revision survived rollback")
	}
	got, e := st.GetGitScan(ctx, repo, j.ID)
	if e != nil || got.TreeIndexed || got.Version != j.Version {
		t.Fatal("fence survived rollback", got, e)
	}
	if e = st.FinishGitScan(ctx, p); e != nil {
		t.Fatal("replay", e)
	}
}
