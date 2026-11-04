package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestLiveObserverUsesInjectedRuntime(test *testing.T) {
	cwd := test.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	failure := errors.New("observer failed")
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, result := range []error{nil, failure, context.Canceled} {
			calls := 0
			container := &Container{
				ObserveLiveSession: func(observedCtx context.Context, observedCwd string, observedProvider domain.ProviderKind, id string) error {
					calls++
					if observedCtx != ctx || observedCwd != cwd || observedProvider != provider || id != sessionID {
						test.Fatal("live-watch changed the observed invocation")
					}
					return result
				},
				WakeFetchDiscovery: func(string) { test.Fatal("driver woke discovery outside observer") },
			}
			if err := runGitHook(ctx, container, cwd, []string{"live-watch", string(provider), sessionID}); !errors.Is(err, result) || calls != 1 {
				test.Fatal("live-watch bypassed its injected runtime", calls, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		test.Fatal("driver created observer state", err)
	}
}

func TestLiveObserverRejectsMissingCompositionAndInvalidArgs(test *testing.T) {
	cwd := test.TempDir()
	for _, container := range []*Container{nil, {}} {
		if err := runGitHook(context.Background(), container, cwd, []string{"live-watch", domain.ProviderCodex, "registered"}); err == nil || !strings.Contains(err.Error(), "live session observer unavailable") {
			test.Fatal("missing composition silently created an observer", err)
		}
	}
	container := &Container{ObserveLiveSession: func(context.Context, string, domain.ProviderKind, string) error {
		test.Fatal("invalid arguments reached observer")
		return nil
	}}
	for _, args := range [][]string{nil, {domain.ProviderCodex}, {domain.ProviderCodex, "registered", "extra"}} {
		if err := runGitHook(context.Background(), container, cwd, append([]string{"live-watch"}, args...)); err == nil || !strings.Contains(err.Error(), "live-watch requires provider and registered session") {
			test.Fatal("live-watch argument contract changed", err)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		test.Fatal("rejected observer created state", err)
	}
}

func TestLiveCaptureUsesRootCoordinatorWithoutChangingObserverFlags(test *testing.T) {
	cwd, container, _, _, _ := historyFixture(test)
	home := test.TempDir()
	test.Setenv("HOME", home)
	test.Setenv("CODEX_HOME", "")
	const sessionID = "11111111-1111-4111-8111-111111111111"
	path := writeCodexRollout(test, home, cwd, sessionID, time.Now())
	if err := capture.TrackAppSession(cwd, domain.ProviderCodex, sessionID, path); err != nil {
		test.Fatal(err)
	}
	failure := errors.New("capture coordinator refused source")
	for _, result := range []error{nil, failure} {
		calls := 0
		container.RequestCapture = func(_ context.Context, provider domain.ProviderKind, observedCwd, observedPath, id string, debounce, force bool) (bool, error) {
			calls++
			if provider != domain.ProviderCodex || observedCwd != cwd || observedPath != path || id != sessionID || debounce || force {
				test.Fatal("live observer changed capture scope or gates", provider, observedCwd, observedPath, id, debounce, force)
			}
			return false, result
		}
		err := runLiveCapture(context.Background(), container, cwd, []string{domain.ProviderCodex, sessionID})
		if !errors.Is(err, result) || calls != 1 {
			test.Fatal("live capture bypassed its root coordinator", calls, err)
		}
	}
	container.RequestCapture = nil
	if err := runLiveCapture(context.Background(), container, cwd, []string{domain.ProviderCodex, sessionID}); err == nil || !strings.Contains(err.Error(), "live capture coordinator unavailable") {
		test.Fatal("missing live capture composition created an implicit coordinator", err)
	}
}

func TestLiveCaptureUnregisteredSessionDoesNotCreateState(test *testing.T) {
	cwd := test.TempDir()
	err := runLiveCapture(context.Background(), nil, cwd, []string{domain.ProviderCodex, "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		test.Fatal("unregistered live request created state", err)
	}
}
