//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Run real signals only in a separate process group, never in the go test
// process. Both the foreground TUI and isolated preparation helper are fakes.
func TestProviderLaunchSignals(t *testing.T) {
	for _, tt := range []struct {
		mode string
		sig  syscall.Signal
	}{
		{"prepare", syscall.SIGINT}, {"prepare", syscall.SIGTERM},
		{"owned", syscall.SIGINT}, {"owned", syscall.SIGTERM},
		{"late", syscall.SIGINT},
		{"native", syscall.SIGINT}, {"native", syscall.SIGTERM},
		{"replacement", syscall.SIGINT}, {"replacement-failed", syscall.SIGINT},
		{"direct", syscall.SIGINT},
		{"direct-interrupt-exit", syscall.SIGINT},
	} {
		t.Run(tt.mode+"/"+tt.sig.String(), func(t *testing.T) {
			root, _ := providerLaunchFixture(t, "claude", "export CXT_TEST_SIGNAL_ROLE=tui\nexec '"+strings.ReplaceAll(os.Args[0], "'", "'\\''")+"' -test.run='^TestProviderLaunchSignalProcess$'")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestProviderLaunchSignalProcess$")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CXT_TEST_SIGNAL_ROLE=wrapper", "CXT_TEST_SIGNAL_ROOT="+root, "CXT_TEST_SIGNAL_MODE="+tt.mode)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			defer func() {
				// Also contain a regression which kills the wrapper before Cleanup.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				for i := 0; i < 2; i++ {
					if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("cleanup-%d", i))); err == nil {
						continue
					}
					raw, _ := os.ReadFile(filepath.Join(root, fmt.Sprintf("helper-%d.pid", i)))
					if pid, err := strconv.Atoi(string(raw)); err == nil && pid > 0 {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
				}
				<-done
			}()
			waitFile := func(name string) {
				t.Helper()
				if err := waitProviderSignalFile(ctx, root, name, done); err != nil {
					select {
					case <-done:
						t.Fatalf("waiting for %s: %v; process=%v\n%s", name, err, waitErr, output.String())
					default:
						t.Fatalf("waiting for %s: %v", name, err)
					}
				}
			}
			mark := func(name string) {
				t.Helper()
				if err := markProviderSignalFile(root, name); err != nil {
					t.Fatal(err)
				}
			}
			send := func(sig syscall.Signal) {
				t.Helper()
				if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
					t.Fatal(err)
				}
			}
			assertNativeSurvived := func() {
				t.Helper()
				waitFile("native-interrupt")
				select {
				case <-done:
					t.Fatalf("native SIGINT retired wrapper: %v\n%s", waitErr, output.String())
				case <-time.After(100 * time.Millisecond):
				}
				if _, err := os.Stat(filepath.Join(root, "cleanup-0")); !os.IsNotExist(err) {
					t.Fatalf("native SIGINT cleaned current runtime: %v", err)
				}
			}
			switch tt.mode {
			case "prepare":
				waitFile("preparing")
				send(tt.sig)
			case "owned", "late":
				waitFile("starting-0")
				send(tt.sig)
			case "native", "direct":
				waitFile("native-ready")
				if tt.mode == "native" {
					// Start has handed off the terminal but has not returned yet.
					waitFile("handoff")
				}
				send(tt.sig)
				if tt.sig == syscall.SIGINT {
					assertNativeSurvived()
					if tt.mode == "native" {
						mark("return-start")
						waitFile("launched")
					}
					send(syscall.SIGTERM)
				}
			case "direct-interrupt-exit":
				waitFile("native-ready")
				send(syscall.SIGINT)
			case "replacement", "replacement-failed":
				waitFile("native-ready")
				waitFile("launched")
				if err := boundary.Record(root, boundary.Boundary{PrevBranch: "main", Branch: "feature", SeedID: launchSessionID}); err != nil {
					t.Fatal(err)
				}
				waitFile("replacement-preparing")
				send(syscall.SIGINT)
				assertNativeSurvived()
				mark("finish-preparation")
				if tt.mode == "replacement-failed" {
					waitFile("cleanup-1")
					if err := os.Remove(filepath.Join(root, "native-interrupt")); err != nil {
						t.Fatal(err)
					}
					send(syscall.SIGINT)
					assertNativeSurvived()
					send(syscall.SIGTERM)
				} else {
					waitFile("starting-1")
					waitFile("cleanup-0")
					// SIGINT is CXT-owned again after the old TUI was joined.
					send(syscall.SIGINT)
				}
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatalf("wrapper failed: %v\n%s", waitErr, output.String())
				}
			case <-ctx.Done():
				t.Fatal("signal did not complete joined cleanup")
			}
			waitFile("completed")
		})
	}
}

func TestProviderLaunchSignalProcess(t *testing.T) {
	role, root := os.Getenv("CXT_TEST_SIGNAL_ROLE"), os.Getenv("CXT_TEST_SIGNAL_ROOT")
	if role == "" {
		return
	}
	if role == "helper" {
		time.Sleep(30 * time.Second)
		return
	}
	if role == "tui" {
		signals := make(chan os.Signal, 4)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		if err := markProviderSignalFile(root, "native-ready"); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case sig := <-signals:
				if sig == syscall.SIGTERM {
					return
				}
				if err := markProviderSignalFile(root, "native-interrupt"); err != nil {
					t.Fatal(err)
				}
				if os.Getenv("CXT_TEST_SIGNAL_MODE") == "direct-interrupt-exit" {
					return
				}
			case <-time.After(20 * time.Second):
				t.Fatal("fake TUI was not retired")
			}
		}
	}
	if err := runProviderSignalFixture(root, os.Getenv("CXT_TEST_SIGNAL_MODE")); err != nil {
		t.Fatal(err)
	}
	if err := markProviderSignalFile(root, "completed"); err != nil {
		t.Fatal(err)
	}
}

func runProviderSignalFixture(root, mode string) error {
	// The public direct path also verifies public signal installation without
	// requiring a real terminal, provider, user configuration, or native adapter.
	if mode == "direct" || mode == "direct-interrupt-exit" {
		err := RunProviderLaunch(context.Background(), root, LaunchIntent{Provider: domain.ProviderClaude, ProviderArgs: []string{"--help"}}, ProviderLaunchHooks{})
		if mode == "direct-interrupt-exit" {
			if err != nil {
				return fmt.Errorf("public native SIGINT clean exit: %w", err)
			}
			return nil
		}
		if !errors.Is(err, context.Canceled) {
			return fmt.Errorf("public direct launch did not cancel: %w", err)
		}
		return nil
	}
	calls, cleanups, launched := 0, 0, false
	hooks := ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, r ProviderLaunchRequest) (DeferredProviderLaunch, error) {
			index := calls
			calls++
			helper := exec.Command(os.Args[0], "-test.run=^TestProviderLaunchSignalProcess$")
			helper.Env = append(os.Environ(), "CXT_TEST_SIGNAL_ROLE=helper")
			helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := helper.Start(); err != nil {
				return DeferredProviderLaunch{}, err
			}
			var child *exec.Cmd
			var attempted, joined bool
			d := deferredLaunchFixture(ctx, r, func() error {
				cleanups++
				_ = helper.Process.Kill()
				_ = helper.Wait()
				if attempted && !joined || child != nil && child.ProcessState == nil {
					return errors.New("cleanup ran before starter/child was joined")
				}
				return markProviderSignalFile(root, fmt.Sprintf("cleanup-%d", index))
			})
			d.SessionID = r.NativeSessionID()
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("helper-%d.pid", index)), []byte(strconv.Itoa(helper.Process.Pid)), 0600); err != nil {
				return d, err
			}
			if mode == "prepare" {
				if err := markProviderSignalFile(root, "preparing"); err != nil {
					return d, err
				}
				<-ctx.Done()
				return d, ctx.Err()
			}
			if index == 1 {
				if err := markProviderSignalFile(root, "replacement-preparing"); err != nil {
					return d, err
				}
				if err := waitProviderSignalFile(ctx, root, "finish-preparation", nil); err != nil {
					return d, err
				}
				if mode == "replacement-failed" {
					return d, errors.New("synthetic replacement preparation failure")
				}
			}
			d.Start = func(ctx context.Context, in io.Reader, out, stderr io.Writer, terminalStarted func()) (*exec.Cmd, error) {
				attempted = true
				defer func() { joined = true }()
				if err := markProviderSignalFile(root, fmt.Sprintf("starting-%d", index)); err != nil {
					return nil, err
				}
				if mode == "owned" || mode == "late" || index == 1 {
					<-ctx.Done()
					if mode != "late" {
						return nil, ctx.Err()
					}
				}
				var err error
				child, err = ownedStartTestCommand(r, in, out, stderr, terminalStarted)
				if err != nil {
					return child, err
				}
				if mode == "late" {
					if ctx.Err() == nil {
						return child, errors.New("terminal handoff cleared cancellation")
					}
					return child, ctx.Err()
				}
				if mode == "native" {
					if err := markProviderSignalFile(root, "handoff"); err != nil {
						return child, err
					}
					if err := waitProviderSignalFile(ctx, root, "return-start", nil); err != nil {
						return child, err
					}
				}
				return child, nil
			}
			return d, nil
		},
		Record: func(_ context.Context, r ProviderLaunchReceipt) error {
			if r.State == "runtime_launched" {
				launched = true
				return markProviderSignalFile(root, "launched")
			}
			return nil
		},
	}
	runtime := launchTestRuntime()
	runtime.handleSignals = true
	err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}, hooks, runtime)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errProviderCleanup) || cleanups != calls {
		return fmt.Errorf("cancellation=%v; cleanups=%d preparations=%d", err, cleanups, calls)
	}
	if (mode == "owned" || mode == "prepare" || mode == "late") && launched {
		return errors.New("canceled starter claimed runtime_launched")
	}
	return nil
}

func markProviderSignalFile(root, name string) error {
	return os.WriteFile(filepath.Join(root, name), nil, 0600)
}

func waitProviderSignalFile(ctx context.Context, root, name string, exited <-chan struct{}) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return errors.New("process exited before expected marker")
		case <-ticker.C:
		}
	}
}
