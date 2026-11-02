package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func repositoryResolverForTest(root string) func(context.Context, string) (domain.Repo, error) {
	return remotecfg.Wrap(root, gitctx.NewGitContextAdapter()).CurrentRepo
}

func TestLocalRepositoryResolverHasNoImplicitFallback(test *testing.T) {
	ctx := context.Background()
	cwd, container, _, _, _ := historyFixture(test)
	for _, missing := range []*Container{nil, {}} {
		if _, err := resolveLocalRepo(ctx, missing, cwd); err == nil || !strings.Contains(err.Error(), "resolver unavailable") {
			test.Fatalf("missing composition used an implicit resolver: %v", err)
		}
	}
	blocked := errors.New("repository identity unavailable")
	calls := 0
	container.ResolveRepo = func(current context.Context, path string) (domain.Repo, error) {
		if current != ctx || path != cwd {
			test.Fatal("repository observation changed")
		}
		calls++
		return domain.Repo{}, blocked
	}
	if _, err := prepareBranchHistory(ctx, container, cwd, "feature", gitOut(cwd, "rev-parse", "HEAD"), false); !errors.Is(err, blocked) || calls != 1 {
		test.Fatalf("branch birth bypassed injected identity: calls=%d error=%v", calls, err)
	}
}

type historicalCompositionSync struct {
	inbound.SyncRepo
	calls int
	input inbound.SyncInput
	limit int
}

func (syncer *historicalCompositionSync) SyncHistorical(_ context.Context, input inbound.SyncInput, limit int) (inbound.HistoricalSyncOutput, error) {
	syncer.calls++
	syncer.input, syncer.limit = input, limit
	return inbound.HistoricalSyncOutput{}, nil
}

func TestHistoricalSyncUsesInjectedDaemonExclusion(test *testing.T) {
	test.Setenv("CXT_REMOTE", "https://api.example.test")
	for _, state := range []string{"missing", "busy", "failure", "acquired"} {
		test.Run(state, func(test *testing.T) {
			cwd := test.TempDir()
			syncer := &historicalCompositionSync{}
			container := &Container{Sync: syncer}
			blocked := errors.New("daemon lock failed")
			if state != "missing" {
				container.WithBackfillDaemon = func(ctx context.Context, root string, work func() error) (bool, error) {
					deadline, ok := ctx.Deadline()
					if root != cwd || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute {
						test.Fatal("daemon scope or lifetime changed")
					}
					if state == "busy" {
						return false, nil
					}
					if state == "failure" {
						return false, blocked
					}
					return true, work()
				}
			}
			err := runHistoricalSync(context.Background(), container, cwd)
			switch state {
			case "missing":
				if err == nil || !strings.Contains(err.Error(), "lock unavailable") {
					test.Fatalf("missing composition accepted: %v", err)
				}
			case "failure":
				if !errors.Is(err, blocked) {
					test.Fatalf("lock failure lost: %v", err)
				}
			default:
				if err != nil {
					test.Fatal(err)
				}
			}
			if state == "acquired" {
				if syncer.calls != 1 || syncer.input.Cwd != cwd || syncer.limit != 8 {
					test.Fatalf("historical work changed: %+v", syncer)
				}
			} else if syncer.calls != 0 {
				test.Fatal("historical work ran without lock ownership")
			}
			if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !errors.Is(err, os.ErrNotExist) {
				test.Fatalf("driver constructed a fallback store: %v", err)
			}
		})
	}
}
