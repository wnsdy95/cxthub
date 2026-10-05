package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestProviderLaunchSignalsCloseSettlesPending(t *testing.T) {
	for _, tt := range []struct {
		name      string
		native    bool
		pending   []os.Signal
		wantCalls int
		wantErr   error
	}{
		{name: "normal", native: true, wantCalls: 1},
		{name: "native-interrupt", native: true, pending: []os.Signal{os.Interrupt}, wantCalls: 1},
		{name: "native-terminate", native: true, pending: []os.Signal{os.Interrupt, syscall.SIGTERM}, wantCalls: 2, wantErr: context.Canceled},
		{name: "owned-interrupt", pending: []os.Signal{os.Interrupt}, wantCalls: 2, wantErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, s := ownProviderLaunchSignals(context.Background())
			// Join the receiver before queuing anything: this deterministically
			// models it choosing stop instead of receiving the pending signal.
			// No OS signal or scheduler timing participates in this regression.
			signal.Stop(s.signals)
			close(s.stop)
			<-s.done
			s.stop = make(chan struct{})
			if tt.native {
				s.terminalStarted()
			}
			for _, sig := range tt.pending {
				s.signals <- sig
			}
			if err := ctx.Err(); err != nil {
				t.Fatalf("context canceled before shutdown: %v", err)
			}
			cancel, calls := s.cancel, 0
			s.cancel = func() { calls++; cancel() }
			if err := s.close(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("settled shutdown error=%v; want %v", err, tt.wantErr)
			}
			if len(s.signals) != 0 || calls != tt.wantCalls {
				t.Fatalf("pending=%d cancel calls=%d; want pending=0 calls=%d (including cleanup)", len(s.signals), calls, tt.wantCalls)
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("cleanup did not release context: %v", ctx.Err())
			}
		})
	}
}

func TestProviderLaunchPublicExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		code int
	}{
		{"normal", "exit 0", 0},
		{"failure", "exit 7", 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", tt.body)
			err := RunProviderLaunch(context.Background(), root, LaunchIntent{Provider: domain.ProviderClaude, ProviderArgs: []string{"--help"}}, ProviderLaunchHooks{})
			if tt.code == 0 {
				if err != nil {
					t.Fatalf("normal exit: %v", err)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.code || errors.Is(err, context.Canceled) {
				t.Fatalf("failure exit: %v; want exit %d without cancellation", err, tt.code)
			}
		})
	}
}

func TestProviderLaunchSignalsPreserveCleanupResult(t *testing.T) {
	cleanupFailure := errors.New("synthetic cleanup failure")
	for _, tt := range []struct {
		name       string
		cancel     bool
		cleanupErr error
	}{
		{name: "normal"},
		{name: "cleanup-failure", cleanupErr: cleanupFailure},
		{name: "cancellation-and-cleanup-failure", cancel: true, cleanupErr: cleanupFailure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "exit 0")
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var launchCtx context.Context
			cleaned := 0
			hooks := ProviderLaunchHooks{
				Prepare: func(ctx context.Context, request ProviderLaunchRequest) (PreparedProviderLaunch, error) {
					launchCtx = ctx
					p := preparedLaunch(request)
					p.Cleanup = func() error {
						cleaned++
						if tt.cancel {
							cancel()
						}
						return tt.cleanupErr
					}
					return p, nil
				},
				Record: func(context.Context, ProviderLaunchReceipt) error { return nil },
			}
			runtime := launchTestRuntime()
			runtime.handleSignals = true
			err := runProviderLaunch(parent, root, LaunchIntent{Provider: domain.ProviderClaude}, hooks, runtime)
			if cleaned != 1 || launchCtx == nil || !errors.Is(launchCtx.Err(), context.Canceled) {
				t.Fatalf("cleanup count=%d context=%v; signal owner was not released", cleaned, launchCtx)
			}
			if errors.Is(err, context.Canceled) != tt.cancel || errors.Is(err, cleanupFailure) != (tt.cleanupErr != nil) {
				t.Fatalf("result=%v; want cancellation=%t cleanup failure=%t", err, tt.cancel, tt.cleanupErr != nil)
			}
			if !tt.cancel && tt.cleanupErr == nil && err != nil {
				t.Fatalf("ordinary cleanup canceled a normal launch: %v", err)
			}
		})
	}
}
