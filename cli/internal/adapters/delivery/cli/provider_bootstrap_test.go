package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func preparedBootstrap(req ProviderLaunchRequest) PreparedProviderLaunch {
	p := preparedLaunch(req)
	p.Bootstrap = &domain.AgentBootstrapProof{Server: domain.EmptyRepositoryProof{RepositoryID: string(domain.HashContent([]byte("empty repo"))), StateHash: domain.HashContent([]byte("empty state"))}, Branch: "main", Unborn: true, WorktreeStateHash: domain.HashContent([]byte("empty worktree"))}
	p.CodeCommit = ""
	p.SourceRevision = string(p.Bootstrap.Server.StateHash)
	p.Capability = "verified_empty_repository"
	p.Validate = func(context.Context) error { return nil }
	return p
}

func TestProviderBootstrapReceiptsAreExplicitAndAcceptanceUnknown(t *testing.T) {
	root, _ := providerLaunchFixture(t, "claude", "")
	var receipts []ProviderLaunchReceipt
	checks := 0
	hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		p := preparedBootstrap(req)
		p.Validate = func(context.Context) error { checks++; return nil }
		return p, nil
	}, Record: func(_ context.Context, r ProviderLaunchReceipt) error { receipts = append(receipts, r); return nil }}
	if err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: domain.ProviderClaude}, hooks, launchTestRuntime()); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || len(receipts) != 2 || receipts[0].State != "prepared" || receipts[1].State != "launched" {
		t.Fatalf("wrong launch lifecycle: %d %+v", checks, receipts)
	}
	for _, r := range receipts {
		if r.Mode != "empty_bootstrap" || r.Bootstrap == nil || !r.Bootstrap.Unborn || r.CodeCommit != "" || r.Acceptance != "unknown" {
			t.Fatalf("dishonest bootstrap receipt: %+v", r)
		}
	}
}

func TestProviderBootstrapRejectsMissingProofOrFailedRevalidation(t *testing.T) {
	for _, mode := range []string{"missing-proof", "missing-validation", "fabricated-sha", "revision", "revoked", "history", "work-state", "transition"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "")
			intent := LaunchIntent{Provider: domain.ProviderClaude}
			if mode == "history" {
				intent.Pull, intent.ContextBudget = true, 200000
			}
			if mode == "work-state" {
				intent.WorkStatePath = "explicit.json"
			}
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				p := preparedBootstrap(req)
				switch mode {
				case "missing-proof":
					p.Bootstrap = nil
				case "missing-validation":
					p.Validate = nil
				case "fabricated-sha":
					p.CodeCommit = "unborn"
				case "revision":
					p.SourceRevision = "different"
				case "revoked":
					p.Validate = func(context.Context) error { return errors.New("403") }
				case "transition":
					req.Transition = &ProviderLaunchTransition{Branch: "other"}
					if err := validatePreparedProviderLaunch(req, p); err == nil {
						t.Fatal("bootstrap allowed for transition")
					}
					return PreparedProviderLaunch{}, errors.New("transition rejected")
				}
				return p, nil
			}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
			if err := runProviderLaunch(context.Background(), root, intent, hooks, launchTestRuntime()); err == nil {
				t.Fatal("unsafe bootstrap launched")
			}
			if _, err := os.Stat(filepath.Join(root, "launch.log")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("provider started after failed proof: %v", err)
			}
		})
	}
}
