package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type advancingRepairRemote struct {
	*causalPullRemote
	later domain.Ref
}

func (r advancingRepairRemote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	return domain.Manifest{RepoID: r.later.RepoID, Refs: []domain.Ref{r.later}}, nil
}

func TestRepairUsesRefsFromTheVerifiedPullObservation(t *testing.T) {
	for _, fetchOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "fetch-only", false: "adopting"}[fetchOnly], func(t *testing.T) {
			ctx := context.Background()
			repo := string(domain.HashContent([]byte(t.Name())))
			doc := pullDoc(t, "verified before concurrent publication")
			snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}
			later := domain.Ref{Kind: domain.RefBranch, RepoID: repo, Name: "main", Target: domain.HashContent([]byte("published after pull"))}
			remote := advancingRepairRemote{&causalPullRemote{snapshot: snap, doc: doc}, later}
			stage := storage.NewFileStore(t.TempDir())
			fetched, err := newTestSyncService(stage, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: fetchOnly})
			if err != nil {
				t.Fatal(err)
			}
			if len(fetched.FetchedRefs) != 1 || fetched.FetchedRefs[0].Target != snap.ID {
				t.Fatal("lost the verified pull observation", fetched)
			}
			if fetchOnly {
				if _, err := stage.GetRef(ctx, repo, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("fetch-only moved a local ref", err)
				}
			}
			// Reproduce the old command's second manifest read. It cannot safely
			// describe the already-verified staging directory.
			newer, err := remote.RemoteManifest(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			local := storage.NewFileStore(t.TempDir())
			if _, err := local.RepairFromReplica(ctx, stage, repo, newer.Refs, t.TempDir()); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("newer unfetched ref accepted", err)
			}
			if _, err := local.RepairFromReplica(ctx, stage, repo, fetched.FetchedRefs, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			got, err := local.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || got.Target != snap.ID {
				t.Fatal("repair invented or lost the selected observation", err)
			}
			if _, err := local.GetSnapshot(ctx, later.Target); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("repair fabricated later context", err)
			}
		})
	}
}
