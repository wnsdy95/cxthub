package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type branchPointerRemote struct {
	outbound.RemoteSync // Any object transfer or write is forbidden in this test.
	ref                 domain.Ref
	err                 error
}

func (r branchPointerRemote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	return domain.Manifest{Refs: []domain.Ref{r.ref}}, r.err
}

func TestReadRemoteBranchDoesNotSynchronizeObjects(t *testing.T) {
	repo := string(gh('r'))
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: gh('t')}
	for _, mode := range []string{"missing local target", "missing branch", "wrong repository", "remote failure"} {
		t.Run(mode, func(t *testing.T) {
			remote := branchPointerRemote{ref: ref}
			var want error
			switch mode {
			case "missing branch":
				remote.ref.Name = "other"
				want = domain.ErrNotFound
			case "wrong repository":
				remote.ref.RepoID = string(gh('x'))
				want = domain.ErrHashMismatch
			case "remote failure":
				remote.err = errors.New("offline")
				want = remote.err
			}
			// No local store exists: querying the pointer must not require one.
			svc := NewSyncRepoService(nil, remote, nil, nil)
			got, err := svc.ReadRemoteBranch(context.Background(), inbound.SyncInput{RepoID: repo}, "main")
			if !errors.Is(err, want) || (want == nil && got != ref) {
				t.Fatalf("ref=%+v err=%v, want %v", got, err, want)
			}
		})
	}
}
