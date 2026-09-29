package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestInitialPullSelectsOnlyExactAuthorizedCode(t *testing.T) {
	for _, mode := range []string{"exact", "missing-proof", "different-code", "different-branch", "reused-identity", "revoked", "memory-revoked", "existing", "orphan", "position-race", "code-race", "stale-process-code", "stale-process-branch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			svc, store, remote, memory, git, plan := selectedPullFixture(t)
			ref := domain.Ref{RepoID: plan.RepoID, Kind: domain.RefBranch, Name: "main", BranchID: "main-identity", Target: plan.Context.Position}
			if err := store.CompareAndSwapPulledRef(ctx, nil, ref); err != nil {
				t.Fatal(err)
			}
			position := *plan.Expected.Position
			if mode != "existing" {
				position.Snapshot, position.MemoryHash, position.MemorySource, position.Selection = "", "", "", nil
				position.MemoryPinned = false
			}
			position.Orphan = mode == "orphan"
			if err := store.PutWorkingPosition(ctx, position); err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadCheckoutState(ctx, plan.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale-process-code" {
				git.sha = strings.Repeat("d", 40)
			}
			if mode == "stale-process-branch" {
				git.branch = "other"
				other := ref
				other.Name = git.branch
				if err := store.CompareAndSwapPulledRef(ctx, nil, other); err != nil {
					t.Fatal(err)
				}
				ref = other
				remote.view.Branch = git.branch
			}
			event := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: plan.RepoID, BranchID: ref.BranchID, Branch: ref.Name, Kind: "publish", Source: ref.Target, Target: ref.Target, GitAfter: git.sha, CreatedAt: time.Now().UTC()}
			switch mode {
			case "different-code":
				event.GitAfter = strings.Repeat("c", 40)
			case "different-branch":
				event.Branch, event.BranchID = "other", "other-identity"
			case "reused-identity":
				event.BranchID = "old-identity"
			case "revoked":
				remote.err = errors.New("permission revoked")
			case "memory-revoked":
				memory.failAfter = memory.calls
			}
			if mode != "missing-proof" {
				remote.view.History = []domain.HistoryEvent{event}
			}
			if mode == "code-race" || mode == "position-race" {
				svc.memory = promptReadFunc(func(ctx context.Context, repo string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
					if mode == "code-race" {
						git.sha = strings.Repeat("e", 40)
					} else {
						p := position
						p.Rewound = true
						if err := store.PutWorkingPosition(ctx, p); err != nil {
							return domain.EffectiveMemoryPage{}, err
						}
					}
					return memory.QueryEffectiveMemory(ctx, repo, req)
				})
			}
			err = svc.InitializeSelection(ctx, SelectedPullInput{Cwd: git.repo.LocalPath})
			after, readErr := store.ReadCheckoutState(ctx, plan.RepoID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "exact" {
				if err != nil || after.Position.Snapshot != ref.Target || after.Position.GitCommit != git.sha || after.Position.Rewound {
					t.Fatalf("initial selection: %+v %v", after, err)
				}
				prepared, err := svc.Preview(ctx, SelectedPullInput{Cwd: git.repo.LocalPath})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Apply(ctx, git.repo.LocalPath, prepared); err != nil {
					t.Fatal(err)
				}
			} else if mode == "existing" {
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("existing selection changed", err)
				}
			} else {
				if err == nil || after.Position.Snapshot != "" {
					t.Fatalf("unverified initial selection: %+v %v", after, err)
				}
				if mode != "position-race" && !reflect.DeepEqual(before, after) {
					t.Fatal("failed initialization changed the cursor")
				}
			}
		})
	}
}
