//go:build darwin || linux

package nativecodex

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func lifecycleCommand(ctx context.Context, script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", script, "lifecycle-test"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd
}

func lifecycleAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned process did not finish promptly")
	}
}

func TestOwnedProcessImmediateExit(t *testing.T) {
	// Exercises exit before kqueue registration as well as after it. The sole
	// Cmd.Wait must still receive the child's real status: observation may not
	// reap the child first.
	for range 40 {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := lifecycleCommand(ctx, "exit 7")
		p, err := startOwnedProcess(cmd, t.TempDir())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		lifecycleAwait(t, p.done)
		cancel()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 {
			t.Fatalf("exit observer consumed status: %v", cmd.ProcessState)
		}
		if err := p.stop(); err != nil {
			t.Fatal(err)
		}
		if err := p.signal(true); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("signal after reaping: %v", err)
		}
		if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Cancel after reaping: %v", err)
		}
	}
}

type lateExitObserver struct {
	processExitObserver
	register <-chan struct{}
}

func (o *lateExitObserver) wait(pid int) error {
	<-o.register
	return o.processExitObserver.wait(pid)
}

func TestOwnedProcessExitBeforeObserverRegistration(t *testing.T) {
	observer, err := newProcessExitObserver()
	if err != nil {
		t.Fatal(err)
	}
	register := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(register) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := lifecycleCommand(ctx, "exit 11")
	p, err := startObservedProcess(cmd, t.TempDir(), &lateExitObserver{observer, register})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = p.stop() })
	// A second non-reaping observer proves exit occurred before the lifecycle
	// observer registers. The PID stays reserved throughout both observations.
	witness, err := newProcessExitObserver()
	if err != nil {
		t.Fatal(err)
	}
	err = witness.wait(cmd.Process.Pid)
	witness.close()
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	lifecycleAwait(t, p.done)
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 11 {
		t.Fatalf("lost exit before registration: %v", cmd.ProcessState)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedProcessCancellationWhileObserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := lifecycleCommand(ctx, "exec /bin/sleep 30")
	cmd.WaitDelay = time.Nanosecond // must not leave os/exec's unguarded kill path enabled
	p, err := startOwnedProcess(cmd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.stop() })
	if cmd.WaitDelay != 0 {
		t.Fatal("os/exec kill timer remains enabled")
	}
	cancel()
	lifecycleAwait(t, p.done)
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
}

type gatedExitObserver struct {
	processExitObserver
	observed chan struct{}
	release  chan struct{}
}

func (o *gatedExitObserver) wait(pid int) error {
	err := o.processExitObserver.wait(pid)
	close(o.observed)
	<-o.release
	return err
}

func TestOwnedProcessRetirementSerializesReapingAndCancel(t *testing.T) {
	observer, err := newProcessExitObserver()
	if err != nil {
		t.Fatal(err)
	}
	gated := &gatedExitObserver{observer, make(chan struct{}), make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(gated.release) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := lifecycleCommand(ctx, "exit 9")
	p, err := startObservedProcess(cmd, t.TempDir(), gated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = p.stop() })
	lifecycleAwait(t, gated.observed)

	// Model an in-progress signal/peer check. An exit has been observed, but
	// reaping cannot start until every operation holding this lock finishes.
	p.state.mu.Lock()
	unblock()
	cancel() // watchCtx's Cancel competes with the reaping barrier
	select {
	case <-p.done:
		p.state.mu.Unlock()
		t.Fatal("reaped while a lifecycle operation held the lock")
	case <-time.After(20 * time.Millisecond):
	}
	p.state.mu.Unlock()
	lifecycleAwait(t, p.done) // Wait must not deadlock joining the Cancel callback
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 9 {
		t.Fatalf("unexpected exit status: %v", cmd.ProcessState)
	}
	if err := p.signal(false); !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	if p.ownsPeer(nil) {
		t.Fatal("retired process accepted a peer")
	}
}

func TestOwnedProcessPeerChecksRaceWithShutdown(t *testing.T) {
	s, _ := startFixture(t, "")
	conn, err := net.Dial("unix", filepath.Join(s.process.dir, "rpc.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !s.process.ownsPeer(conn) {
		t.Fatal("live owned peer rejected")
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			for range 100 {
				_ = s.process.ownsPeer(conn)
			}
		})
	}
	workers.Go(func() { _ = s.process.cmd.Cancel() })
	workers.Go(func() { _ = s.process.signal(false) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if s.process.ownsPeer(conn) {
		t.Fatal("reaped process accepted a socket with its former PID")
	}
}

type failingExitObserver struct{ closed chan struct{} }

func (o *failingExitObserver) wait(int) error { return errors.New("PRIVATE_OBSERVER_SENTINEL") }
func (o *failingExitObserver) close()         { close(o.closed) }

func TestOwnedProcessObserverFailureKillsAndReaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &failingExitObserver{make(chan struct{})}
	cmd := lifecycleCommand(ctx, "exec /bin/sleep 30")
	p, err := startObservedProcess(cmd, t.TempDir(), observer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.stop() })
	lifecycleAwait(t, p.done)
	lifecycleAwait(t, observer.closed)
	if err := p.stop(); !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("observation failure not redacted: %v", err)
	}
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Fatal("transport directory remained after observer failure")
	}
}

func TestOwnedProcessStartFailureClosesObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &failingExitObserver{make(chan struct{})}
	cmd := exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-executable"))
	if _, err := startObservedProcess(cmd, t.TempDir(), observer); err == nil {
		t.Fatal("missing executable started")
	}
	lifecycleAwait(t, observer.closed)
}

func TestOwnedProcessEscalatesIgnoredTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	cmd := lifecycleCommand(ctx, `trap '' TERM; printf ready > "$1"; exec /bin/sleep 30`, ready)
	p, err := startOwnedProcess(cmd, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.stop() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not install signal disposition")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("termination was not escalated: %v", status)
	}
}
