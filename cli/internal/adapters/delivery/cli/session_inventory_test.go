package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type recordingSessionInventory struct {
	outbound.ProviderSessionInventory
	active, files int
}

func (inventory *recordingSessionInventory) LocateActiveSession(ctx context.Context, cwd string, provider domain.ProviderKind) (string, error) {
	inventory.active++
	return inventory.ProviderSessionInventory.LocateActiveSession(ctx, cwd, provider)
}

func (inventory *recordingSessionInventory) SessionFilesForCwd(ctx context.Context, cwd string, provider domain.ProviderKind) []string {
	inventory.files++
	return inventory.ProviderSessionInventory.SessionFilesForCwd(ctx, cwd, provider)
}

func TestContextSwitchMissingSessionInventoryPreservesSession(test *testing.T) {
	test.Setenv("CXT_KEEP_SESSION", "")
	for _, container := range []*Container{nil, {}, {SessionSelection: captureSelectionForTest()}, {SessionInventory: captureSelectionForTest()}} {
		cwd := test.TempDir()
		path := filepath.Join(cwd, "native-session.jsonl")
		if err := os.WriteFile(path, []byte("preserved native content"), 0600); err != nil {
			test.Fatal(err)
		}
		err := contextSwitch(context.Background(), container, cwd)
		if err == nil || !strings.Contains(err.Error(), "session inventory unavailable") {
			test.Fatal("missing composition silently continued a context transition", err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "preserved native content" {
			test.Fatal("missing composition changed the current session", err)
		}
		if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
			test.Fatal("missing composition created transition state", err)
		}
	}
}

func TestProviderSessionInventoryMissingPreventsManagedLaunch(test *testing.T) {
	cwd, _ := providerLaunchFixture(test, "claude", "")
	prepared, recorded := false, false
	hooks := ProviderLaunchHooks{
		Prepare: func(_ context.Context, request ProviderLaunchRequest) (PreparedProviderLaunch, error) {
			prepared = true
			return preparedLaunch(request), nil
		},
		Record: func(context.Context, ProviderLaunchReceipt) error { recorded = true; return nil },
	}
	runtime := launchTestRuntime()
	runtime.sessionInventory = nil
	err := runProviderLaunch(context.Background(), cwd, LaunchIntent{Provider: domain.ProviderClaude}, hooks, runtime)
	if err == nil || !strings.Contains(err.Error(), "session inventory unavailable") || prepared || recorded {
		test.Fatal("missing inventory allowed managed preparation", prepared, recorded, err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "launch.log")); !os.IsNotExist(err) {
		test.Fatal("missing inventory started the provider", err)
	}
}
