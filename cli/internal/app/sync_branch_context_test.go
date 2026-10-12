package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type branchContextRemote struct {
	outbound.RemoteSync
	view      domain.ContextQueryView
	err       error
	selection domain.ContextSelection
	repo      string
}

func (remote *branchContextRemote) QueryContext(_ context.Context, repo string, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	remote.repo, remote.selection = repo, selection
	return remote.view, remote.err
}

func TestRemoteBranchContextReadsServerInclusionWithoutLocalSelection(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	position := domain.HashContent([]byte("main"))
	contribution := domain.HashContent([]byte("late historical source"))
	for _, scenario := range []string{"included", "reused branch", "wrong branch", "wrong position", "missing inclusion", "invalid membership", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			remote := &branchContextRemote{view: domain.ContextQueryView{
				Version: 1, Branch: "main", Position: position, StateHash: domain.HashContent([]byte("revision")),
				Snapshots: []domain.Snapshot{{ID: position, RepoID: repo}, {ID: contribution, RepoID: repo}},
				Inclusion: &domain.BranchContext{BranchID: "main-id", SnapshotID: position, SnapshotIDs: []domain.ContentHash{position, contribution}},
			}}
			switch scenario {
			case "reused branch":
				remote.view.Inclusion.BranchID = "replacement-id"
			case "wrong branch":
				remote.view.Branch = "feature"
			case "wrong position":
				remote.view.Inclusion.SnapshotID = contribution
			case "missing inclusion":
				remote.view.Inclusion = nil
			case "invalid membership":
				remote.view.Inclusion.SnapshotIDs = []domain.ContentHash{position}
			case "denied":
				remote.err = errors.New("permission denied")
			}
			service := NewSyncRepoService(nil, remote, nil, nil)
			view, err := service.ResolveRemoteBranchContext(context.Background(), inbound.SyncInput{RepoID: repo, ExpectedBranchID: "main-id"}, "main")
			if scenario != "included" {
				if err == nil {
					t.Fatal("unverified server inclusion accepted")
				}
				return
			}
			if err != nil || len(view.Inclusion.SnapshotIDs) != 2 {
				t.Fatalf("view=%+v err=%v", view, err)
			}
			if remote.repo != repo || remote.selection != (domain.ContextSelection{Branch: "main", Position: "main", Scope: "current"}) {
				t.Fatalf("query used another selection: %+v", remote.selection)
			}
		})
	}
}
