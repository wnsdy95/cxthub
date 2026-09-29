package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func selectedPullFixture(t *testing.T) (*SelectedPullService, *storage.FileStore, *historyRemote, *agentMemoryFixture, *stagingGit, outbound.SelectedPullPlan) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte(t.Name())))
	g := &stagingGit{repo: domain.Repo{ID: repo, LocalPath: root}, branch: "main", sha: strings.Repeat("a", 40)}
	st := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), g.branch, g.sha)
	id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "selected"}}})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo}
	if err = st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err = st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Snapshot: id, Branch: g.branch, GitCommit: g.sha}); err != nil {
		t.Fatal(err)
	}
	r := &historyRemote{view: domain.ContextQueryView{Version: 1, Revision: domain.RepositoryRevision{Graph: 1}, Branch: "main", Position: id, StateHash: domain.HashContent([]byte("selected projection")), Snapshots: []domain.Snapshot{snap}}}
	m := &agentMemoryFixture{rev: r.view.Revision}
	svc := NewSelectedPullService(st, r, m, g, g, "https://remote.test/api/v1")
	plan, err := svc.Preview(ctx, SelectedPullInput{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, r, m, g, plan
}
func TestSelectedPullPinsProjectionWithoutChangingRawSelection(t *testing.T) {
	svc, st, r, _, g, plan := selectedPullFixture(t)
	ctx := context.Background()
	got, err := svc.Apply(ctx, g.repo.LocalPath, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan.ID != plan.ID {
		t.Fatal("wrong receipt")
	}
	state, err := st.ReadCheckoutState(ctx, plan.RepoID)
	if err != nil || state.Position.Snapshot != plan.Expected.Position.Snapshot {
		t.Fatal("moved worktree")
	}
	r.view.Revision.Pending++ // unrelated live captures do not invalidate committed projection
	retry, err := svc.Apply(ctx, g.repo.LocalPath, plan)
	if err != nil || retry.AppliedAt != got.AppliedAt {
		t.Fatalf("idempotent retry: %+v %v", retry, err)
	}
}
func TestSelectedPullRejectsRevocationCodeOrProjectionChanges(t *testing.T) {
	for _, kind := range []string{"revocation", "memory-revocation", "code", "projection", "index"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, r, m, g, plan := selectedPullFixture(t)
			ctx := context.Background()
			switch kind {
			case "revocation":
				r.err = errors.New("permission revoked")
			case "memory-revocation":
				m.failAfter = m.calls
			case "code":
				g.sha = strings.Repeat("b", 40)
			case "projection":
				r.view.StateHash = domain.HashContent([]byte("changed"))
			case "index":
				plan.Expected.IndexRevision = domain.HashContent([]byte("old index"))
				plan.ID = outbound.SelectedPullPlanID(plan)
			}
			if _, err := svc.Apply(ctx, g.repo.LocalPath, plan); err == nil {
				t.Fatal("stale or unauthorized apply accepted")
			}
			if _, err := st.ReadAppliedPull(ctx, plan.RepoID, plan.Remote); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("failed apply published receipt: %v", err)
			}
		})
	}
}
