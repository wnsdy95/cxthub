package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"strings"
	"testing"
)

type completedPRRemote struct {
	outbound.RemoteSync
	ref   domain.Ref
	err   error
	pulls int
}

func (r *completedPRRemote) PromotePullRequest(context.Context, string, domain.PullRequestMerge) (domain.Ref, error) {
	return r.ref, r.err
}
func (r *completedPRRemote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	r.pulls++
	return domain.Manifest{}, errors.New("post-completion fetch unavailable")
}
func TestCompletedPRSeparatesDurableServerResultFromLocalRefresh(t *testing.T) {
	for _, kind := range []string{"local-fetch", "server-failure", "wrong-branch", "bad-hash"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte("repo")))
			old := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main", Target: domain.HashContent([]byte("local"))}
			if err := store.PutRef(ctx, old); err != nil {
				t.Fatal(err)
			}
			remote := &completedPRRemote{ref: domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main", Target: domain.HashContent([]byte("confirmed"))}}
			switch kind {
			case "server-failure":
				remote.err = errors.New("unconfirmed")
			case "wrong-branch":
				remote.ref.Name = "other"
			case "bad-hash":
				remote.ref.Target = "bad"
			}
			svc := NewSyncRepoService(store, remote, nil)
			err := svc.PromotePullRequest(ctx, inbound.SyncInput{RepoID: repo}, outbound.MergedPullRequest{Number: 225, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeCommitSHA: strings.Repeat("b", 40)})
			deferred := errors.Is(err, domain.ErrPRLocalReconciliation)
			if err == nil || deferred != (kind == "local-fetch") {
				t.Fatalf("completion=%v for %s", err, kind)
			}
			pending, perr := store.PendingPRDeliveries(ctx, repo)
			if perr != nil {
				t.Fatal(perr)
			}
			want := 1
			if kind == "local-fetch" {
				want = 0
			}
			if len(pending) != want {
				t.Fatalf("pending=%d want%d", len(pending), want)
			}
			after, _ := store.GetRef(ctx, repo, domain.RefBranch, "main")
			if after != old {
				t.Fatal("local record overwritten")
			}
		})
	}
}

func (r *completedPRRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.pulls++
	return nil, nil, nil, errors.New("post-completion fetch unavailable")
}
