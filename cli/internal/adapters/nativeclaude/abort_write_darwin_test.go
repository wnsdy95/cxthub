//go:build darwin

package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const abortWriteRole = "CXT_CLAUDE_ABORT_WRITE_ROLE"

func init() {
	switch os.Getenv(abortWriteRole) {
	case "child":
		os.Exit(abortWriteChild(os.Getenv("CXT_CLAUDE_ABORT_WRITE_ROOT")))
	case "owner":
		result := probeAbortWrite(os.Getenv("CXT_CLAUDE_ABORT_WRITE_ROOT"), os.Getenv("CXT_CLAUDE_ABORT_WRITE_CASE"))
		if json.NewEncoder(os.Stdout).Encode(result) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func abortWriteEnv(root, role, scenario string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + root,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "GORACE=atexit_sleep_ms=0",
		abortWriteRole + "=" + role, "CXT_CLAUDE_ABORT_WRITE_ROOT=" + root, "CXT_CLAUDE_ABORT_WRITE_CASE=" + scenario}
}

func abortWriteMarker(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func awaitAbortWriteMarker(root, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if abortWriteMarker(root, name) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// The child consumes one byte to prove that the actual write reached its pipe,
// then stops reading until cancellation has interrupted that write. EOF is an
// observable side effect, analogous to native's deny-and-continue behavior.
// No native binary, credentials, tools, network, or descendants are involved.
func abortWriteChild(root string) int {
	time.AfterFunc(6*time.Second, func() {
		_ = os.WriteFile(filepath.Join(root, "finite-exit"), []byte{1}, 0600)
		os.Exit(0)
	})
	var first [1]byte
	if _, err := io.ReadFull(os.Stdin, first[:]); err != nil {
		return 2
	}
	if err := os.WriteFile(filepath.Join(root, "first-byte"), first[:], 0600); err != nil {
		return 3
	}
	if !awaitAbortWriteMarker(root, "drain", 6*time.Second) {
		return 4
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		return 5
	}
	if err := os.WriteFile(filepath.Join(root, "saw-eof"), []byte{1}, 0600); err != nil {
		return 6
	}
	return 0
}

type abortWriteResult struct {
	Stage          string
	LiveBefore     bool
	SignalDenied   bool
	ObserverFailed bool
	WriteCanceled  bool
	StrictFailure  bool
	CleanupFailure bool
	SawEOF         bool
	FiniteExit     bool
	LeaderAbsent   bool
	ReadersJoined  bool
	InputClosed    bool
}

func probeAbortWrite(root, scenario string) (result abortWriteResult) {
	exe, err := os.Executable()
	if err != nil {
		result.Stage = "executable"
		return
	}
	env := abortWriteEnv(root, "child", scenario)
	var p *process
	if scenario == "denied-observer-error" {
		p, err = abortWriteFailedObserverProcess(exe, root, env)
	} else {
		p, err = startProcess(exe, nil, root, env, func([]byte) error { return nil }, func(error) {})
	}
	if err != nil {
		result.Stage = "start"
		return
	}
	// shutdown is the sole owner of cmd.Wait. If its bounded Wait observation
	// times out, the finite child still reaches that waiter. Kernel absence
	// confirms reaping without racing ProcessState or stealing its Wait.
	defer func() {
		result.StrictFailure = errors.Is(p.shutdown(true), ErrCleanup)
		result.CleanupFailure = errors.Is(p.retirementErr, ErrCleanup)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			_, err := syscall.Getpgid(p.cmd.Process.Pid)
			if errors.Is(err, syscall.ESRCH) {
				result.LeaderAbsent = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		result.ReadersJoined = waitFor(p.outDone, time.Second) && waitFor(p.errDone, time.Second)
		result.SawEOF, result.FiniteExit = abortWriteMarker(root, "saw-eof"), abortWriteMarker(root, "finite-exit")
		if result.LeaderAbsent {
			// Kernel disappearance can precede the waiter's descriptor close.
			// Observe the production close, not the cleanup fallback below; do
			// not inspect ProcessState while the sole waiter may still write it.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, err := p.stdin.Stat(); errors.Is(err, os.ErrClosed) {
					result.InputClosed = true
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !result.InputClosed {
				// Keep failed tests leak-free without turning this failed
				// observation into a successful retirement assertion.
				_ = p.stdin.Close()
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	written := make(chan error, 1)
	writerJoined := false
	defer func() {
		cancel()
		if !writerJoined {
			select {
			case <-written:
			case <-time.After(4 * time.Second):
				result.Stage = "writer-not-joined"
			}
		}
	}()
	go func() {
		raw := bytes.Repeat([]byte{'x'}, 4<<20)
		if strings.HasPrefix(scenario, "kill-before-eof-") {
			session := &Session{process: p}
			if scenario == "kill-before-eof-literal" {
				session.firstQuestion = &firstQuestionState{}
			}
			written <- session.writeFrame(ctx, raw)
		} else {
			written <- p.write(ctx, raw)
		}
	}()
	if !awaitAbortWriteMarker(root, "first-byte", 2*time.Second) {
		result.Stage = "write-not-observed"
		return
	}
	select {
	case <-written:
		writerJoined = true
		result.Stage = "write-not-blocked"
		return
	default:
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", p.cmd.Process.Pid)
	result.LiveBefore = err == nil && info != nil && int(info.Proc.P_pid) == p.cmd.Process.Pid &&
		int(info.Eproc.Pgid) == p.cmd.Process.Pid && info.Proc.P_stat != 5
	if !result.LiveBefore {
		result.Stage = "live-child-precondition"
		return
	}
	if !strings.HasPrefix(scenario, "kill-before-eof") {
		result.SignalDenied = errors.Is(signalProcessGroup(p.cmd, true), syscall.EPERM)
		if !result.SignalDenied {
			result.Stage = "signal-not-denied"
			return
		}
	}
	if scenario == "denied-observer-error" {
		result.ObserverFailed = waitFor(p.exited, time.Second) && p.observationErr != nil
		if !result.ObserverFailed {
			result.Stage = "observer-error-precondition"
			return
		}
	}
	cancel()
	select {
	case err := <-written:
		writerJoined = true
		result.WriteCanceled = errors.Is(err, context.Canceled)
	case <-time.After(3 * time.Second):
		result.Stage = "blocked-write-not-interrupted"
		return
	}
	if err := os.WriteFile(filepath.Join(root, "drain"), []byte{1}, 0600); err != nil {
		result.Stage = "drain-marker"
		return
	}
	if strings.HasPrefix(scenario, "kill-before-eof") {
		// Under the former callback (stdin.Close only), let the finite helper
		// observe EOF before cleanup can mask it by subsequently killing it.
		if !waitFor(p.exited, 2*time.Second) {
			result.Stage = "cancel-did-not-stop-child"
		}
	}
	return
}

// Exercise an actual OS observer failure without replacing production globals.
// The pipes, owned process and shutdown/write methods are real; only the
// fixture-owned kqueue is deliberately closed before its wait registration.
func abortWriteFailedObserverProcess(exe, root string, env []string) (*process, error) {
	observer, err := newProcessExitObserver()
	if err != nil {
		return nil, err
	}
	observerClosed := false
	defer func() {
		if !observerClosed {
			observer.close()
		}
	}()
	var files []*os.File
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	started := false
	defer func() {
		if !started {
			for _, f := range files {
				_ = f.Close()
			}
		}
	}()
	inR, inW, err := pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = root, env, inR, outW, errW
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started = true
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	p := &process{cmd: cmd, stdin: inW, stdout: outR, stderr: errR,
		exited: make(chan struct{}), outDone: make(chan struct{}), errDone: make(chan struct{}), onError: func(error) {}}
	observer.close()
	observerClosed = true
	p.observationErr = observer.wait(cmd.Process.Pid)
	close(p.exited)
	go func() { defer close(p.outDone); defer outR.Close(); _, _ = io.Copy(io.Discard, outR) }()
	go func() { defer close(p.errDone); defer errR.Close(); _, _ = io.Copy(io.Discard, errR) }()
	return p, nil
}

func TestDarwinAbortWriteWithholdsEOF(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Fatal("sandbox-exec is required for the kernel-enforced regression")
	}
	for _, scenario := range []string{"kill-before-eof", "kill-before-eof-no-query", "kill-before-eof-literal", "denied-signal", "denied-observer-error"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			profile := "(version 1)(allow default)(deny network*)"
			if !strings.HasPrefix(scenario, "kill-before-eof") {
				profile += "(deny signal (target others))"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, exe)
			cmd.Dir, cmd.Env = root, abortWriteEnv(root, "owner", scenario)
			raw, err := cmd.Output() // Always Wait for the directly owned fixture.
			if err != nil {
				t.Fatalf("owned synthetic fixture failed: %v", err)
			}
			var got abortWriteResult
			if json.Unmarshal(raw, &got) != nil {
				t.Fatal("invalid metadata-only fixture result")
			}
			if got.Stage != "" || !got.LiveBefore || !got.WriteCanceled || !got.LeaderAbsent || !got.ReadersJoined || !got.StrictFailure {
				t.Fatalf("fixture or ownership failed: %+v", got)
			}
			if !got.InputClosed {
				t.Fatalf("sole waiter left stdin open after actual child reaping: %+v", got)
			}
			if got.SawEOF {
				t.Fatalf("cancellation delivered EOF to a live child: %+v", got)
			}
			if strings.HasPrefix(scenario, "kill-before-eof") {
				if got.CleanupFailure || got.FiniteExit {
					t.Fatalf("confirmed killed/reaped child misclassified: %+v", got)
				}
			} else if !got.SignalDenied || !got.CleanupFailure || !got.FiniteExit || scenario == "denied-observer-error" && !got.ObserverFailed {
				t.Fatalf("unconfirmed termination was not preserved: %+v", got)
			}
		})
	}
}
