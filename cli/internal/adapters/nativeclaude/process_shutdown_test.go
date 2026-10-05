//go:build darwin || linux

package nativeclaude

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func init() {
	mode := os.Getenv("CXT_CLAUDE_EXIT_TEST")
	if mode == "" {
		return
	}
	// A disposable native substitute with a finite tail after protocol EOF.
	// No network, provider configuration, credentials or descendants are used.
	time.AfterFunc(7*time.Second, func() { os.Exit(98) })
	terminated := make(chan os.Signal, 1)
	if mode == "grace-expired" || mode == "abort-during-grace" {
		signal.Notify(terminated, syscall.SIGTERM)
	}
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if mode == "abort-during-grace" {
		_, _ = os.Stdout.WriteString("eof\n")
		<-terminated
	} else if mode == "grace-expired" {
		<-terminated // Deliberately exit zero after forced graceful termination.
	} else if mode != "abort" {
		time.Sleep(1500 * time.Millisecond)
	}
	_ = os.WriteFile(filepath.Join(os.Getenv("HOME"), "flushed"), []byte("done"), 0600)
	if mode == "failed-exit" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeGracefulExitPreservesCleanupTailAndFailure(t *testing.T) {
	for _, mode := range []string{"normal", "failed-exit", "abort", "grace-expired"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			ready := make(chan struct{})
			p, err := startProcess(exe, nil, root, []string{"HOME=" + root, "GORACE=atexit_sleep_ms=0", "CXT_CLAUDE_EXIT_TEST=" + mode}, func(raw []byte) error {
				if string(raw) != "ready" {
					return ErrProtocol
				}
				close(ready)
				return nil
			}, func(error) {})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.shutdown(true) })
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("helper did not start")
			}
			err = p.shutdown(mode == "abort")
			if mode == "normal" && err != nil {
				t.Fatal("normal EOF cleanup tail was interrupted", err)
			}
			if mode != "normal" && !errors.Is(err, ErrCleanup) {
				t.Fatal("failed or forced exit reported success", err)
			}
			_, statErr := os.Stat(filepath.Join(root, "flushed"))
			if mode == "abort" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("abort delivered EOF and continued helper work")
			}
			if mode != "abort" && statErr != nil {
				t.Fatal("graceful cleanup was not allowed to finish", statErr)
			}
			if p.cmd.ProcessState == nil || p.retirementErr != nil {
				t.Fatal("owned process retirement was not confirmed")
			}
		})
	}
}

func TestNativeGracefulExitCanBeAbortedAfterEOF(t *testing.T) {
	for _, scenario := range []string{"process-abort", "session-cancel", "session-failure"} {
		t.Run(scenario, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			ready, eof := make(chan struct{}), make(chan struct{})
			s := &Session{failed: make(chan struct{}), closed: make(chan struct{})}
			p, err := startProcess(exe, nil, root, []string{"HOME=" + root, "GORACE=atexit_sleep_ms=0", "CXT_CLAUDE_EXIT_TEST=abort-during-grace"}, func(raw []byte) error {
				switch string(raw) {
				case "ready":
					close(ready)
				case "eof":
					close(eof)
				default:
					return ErrProtocol
				}
				return nil
			}, s.fail)
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.process = p
			s.mu.Unlock()
			t.Cleanup(func() { _ = p.shutdown(true) })
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("helper did not start")
			}
			closeProcess := s.Close
			if scenario == "process-abort" {
				closeProcess = p.close
			}
			closed := make(chan error, 1)
			go func() { closed <- closeProcess() }()
			select {
			case <-eof:
			case <-time.After(2 * time.Second):
				t.Fatal("graceful close did not deliver EOF")
			}
			// The helper has consumed EOF and will stay alive until signaled.
			// A second shutdown must wake the existing owner, not wait five
			// seconds behind its sync.Once or acquire its own signal/reap path.
			requested := make(chan error, 1)
			want := error(ErrCleanup)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "process-abort":
				go func() { requested <- p.shutdown(true) }()
			case "session-cancel":
				want = context.Canceled
				stop := context.AfterFunc(ctx, func() {
					s.fail(ctx.Err())
					requested <- s.Close()
				})
				defer stop()
				cancel()
			case "session-failure":
				want = ErrProtocol
				go func() {
					s.fail(want)
					requested <- s.Close()
				}()
			}
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for _, done := range []<-chan error{closed, requested} {
				select {
				case err := <-done:
					if !errors.Is(err, want) || scenario != "process-abort" && errors.Is(err, ErrCleanup) {
						t.Fatal("abort lost its cause or misclassified confirmed retirement", err)
					}
				case <-deadline.C:
					t.Fatal("abort waited for normal teardown grace")
				}
			}
			if p.cmd.ProcessState == nil || p.retirementErr != nil {
				t.Fatal("aborted helper was not reaped")
			}
			status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("abort did not kill the owned helper")
			}
			if _, err := os.Stat(filepath.Join(root, "flushed")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("helper continued graceful cleanup after abort")
			}
			// Late requests after reaping only return the recorded result.
			if err := p.shutdown(true); !errors.Is(err, ErrCleanup) {
				t.Fatal("late abort changed the recorded process result", err)
			}
		})
	}
}
