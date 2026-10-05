//go:build darwin

package nativeclaude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const groupTestRole = "CXT_CLAUDE_GROUP_TEST_ROLE"

// Helpers use fresh environments, disposable files and the test executable.
// No native binary, user configuration or replaceable global function is used.
func init() {
	if role := os.Getenv(groupTestRole); role != "" {
		os.Exit(runGroupTestHelper(role))
	}
}

type groupTestResult struct {
	Stage                string
	SignalEPERM          bool
	DescendantLiveBefore bool
	CleanupError         bool
	OtherError           bool
	LeaderReaped         bool
	DescendantAbsent     bool
}

func groupTestEnv(root, role, api, scenario string) []string {
	return []string{
		"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + root,
		"CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "CODEX_HOME=" + filepath.Join(root, "codex"),
		"GORACE=atexit_sleep_ms=0", groupTestRole + "=" + role,
		"CXT_CLAUDE_GROUP_TEST_ROOT=" + root, "CXT_CLAUDE_GROUP_TEST_API=" + api,
		"CXT_CLAUDE_GROUP_TEST_SCENARIO=" + scenario,
	}
}

func runGroupTestHelper(role string) int {
	root := os.Getenv("CXT_CLAUDE_GROUP_TEST_ROOT")
	api, scenario := os.Getenv("CXT_CLAUDE_GROUP_TEST_API"), os.Getenv("CXT_CLAUDE_GROUP_TEST_SCENARIO")
	switch role {
	case "descendant":
		// A two-second lifetime starts before readiness. This child outlives
		// its parent but never inherits the protocol output descriptors.
		timer := time.NewTimer(2 * time.Second)
		ready := os.NewFile(3, "owned-ready")
		if _, err := ready.Write([]byte{1}); err != nil {
			return 1
		}
		_ = ready.Close()
		<-timer.C
		return 0
	case "leader":
		if scenario == "zombie-only" {
			return 0
		}
		exe, err := os.Executable()
		if err != nil {
			return 1
		}
		r, w, err := os.Pipe()
		if err != nil {
			return 1
		}
		defer r.Close()
		defer w.Close()
		child := exec.Command(exe)
		child.Env = groupTestEnv(root, "descendant", api, scenario)
		child.ExtraFiles = []*os.File{w}
		if err := child.Start(); err != nil {
			return 1
		}
		_ = w.Close()
		raw, _ := json.Marshal(child.Process.Pid)
		if err := os.WriteFile(filepath.Join(root, "descendant.json"), raw, 0o600); err != nil {
			_ = child.Wait()
			return 1
		}
		_ = r.SetReadDeadline(time.Now().Add(time.Second))
		var ready [1]byte
		if _, err := r.Read(ready[:]); err != nil || ready[0] != 1 {
			_ = child.Wait()
			return 1
		}
		// Reparenting the finite descendant is the condition under test.
		// The supervisor observes its disappearance without post-reap signals.
		return 0
	case "supervisor":
		if err := json.NewEncoder(os.Stdout).Encode(probeGroupTest(root, api, scenario)); err != nil {
			return 1
		}
		return 0
	default:
		return 1
	}
}

func probeGroupTest(root, api, scenario string) (result groupTestResult) {
	// Join the finite orphan's lifetime on every return path. No numeric PID
	// is ever signaled after its leader's Wait releases the group reservation.
	defer func() {
		if scenario == "zombie-only" {
			result.DescendantAbsent = true
			return
		}
		raw, err := os.ReadFile(filepath.Join(root, "descendant.json"))
		var pid int
		if err != nil || json.Unmarshal(raw, &pid) != nil || pid <= 1 {
			return
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := syscall.Getpgid(pid); err == syscall.ESRCH {
				result.DescendantAbsent = true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		result.Stage = "executable"
		return
	}
	env := groupTestEnv(root, "leader", api, scenario)
	var cmd *exec.Cmd
	var exited <-chan struct{}
	var finish func() error
	var observationError func() error
	if api == "idle" {
		observer, err := newProcessExitObserver()
		if err != nil {
			result.Stage = "observer"
			return
		}
		cmd = exec.Command(exe)
		cmd.Env, cmd.Dir = env, root
		configureProcess(cmd)
		if err := cmd.Start(); err != nil {
			observer.close()
			result.Stage = "idle-start"
			return
		}
		p := &IdleProcess{idleProcessState: &idleProcessState{cmd: cmd, exited: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{})}}
		go func() { p.observationErr = observer.wait(cmd.Process.Pid); observer.close(); close(p.exited) }()
		exited = p.exited
		observationError = func() error { return p.observationErr }
		finish = func() error {
			// Exercise the real lifecycle after natural exit observation.
			go p.run(context.Background())
			return p.Wait(context.Background())
		}
	} else {
		p, err := startProcess(exe, nil, root, env, func([]byte) error { return nil }, func(error) {})
		if err != nil {
			result.Stage = "helper-start"
			return
		}
		cmd, exited, finish = p.cmd, p.exited, p.close
		if api == "abort" {
			finish = func() error { _ = p.shutdown(true); return p.retirementErr }
		}
		observationError = func() error { return p.observationErr }
	}
	// Both actual lifecycle implementations own the mandatory leader Wait.
	defer func() {
		cleanup := finish()
		result.CleanupError = errors.Is(cleanup, ErrCleanup)
		result.OtherError = cleanup != nil && !result.CleanupError
		var status syscall.WaitStatus
		_, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
		result.LeaderReaped = cmd.ProcessState != nil && cmd.ProcessState.Success() && err == syscall.ECHILD
	}()
	if !waitFor(exited, 2*time.Second) {
		result.Stage = "leader-exit-observation"
		return
	}
	if observationError() != nil {
		result.Stage = "leader-exit-observer-error"
		return
	}
	if scenario != "zombie-only" {
		raw, err := os.ReadFile(filepath.Join(root, "descendant.json"))
		var pid int
		if err != nil || json.Unmarshal(raw, &pid) != nil {
			result.Stage = "descendant-record"
			return
		}
		info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		// A retained PID/group can belong to a zombie. Require the kernel's
		// exact member identity and non-SZOMB state before exercising cleanup.
		result.DescendantLiveBefore = err == nil && info != nil && int(info.Proc.P_pid) == pid &&
			int(info.Eproc.Pgid) == cmd.Process.Pid && info.Proc.P_stat != 5
	}
	// The unreaped leader still reserves the group. This is a real denied
	// SIGKILL, not a stub pretending that OS permission checks failed.
	result.SignalEPERM = errors.Is(signalProcessGroup(cmd, true), syscall.EPERM)
	return
}

func TestDarwinProcessGroupCleanup(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Fatal("sandbox-exec is required for the kernel-enforced cleanup regression")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"idle", "helper", "abort"} {
		for _, scenario := range []string{"denied-live-descendant", "zombie-only"} {
			t.Run(api+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				profile := "(version 1)(allow default)(deny network*)(deny signal (target others))"
				cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, exe)
				cmd.Dir, cmd.Env = root, groupTestEnv(root, "supervisor", api, scenario)
				// Output joins/reaps the directly owned supervisor. Its descendants
				// have independent finite lifetimes and isolated metadata.
				raw, err := cmd.Output()
				if err != nil {
					t.Fatalf("owned supervisor failed: %v", err)
				}
				var got groupTestResult
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal("invalid metadata-only supervisor result")
				}
				if got.Stage != "" || !got.LeaderReaped || !got.DescendantAbsent || !got.SignalEPERM || got.OtherError {
					t.Fatalf("fixture/ownership precondition failed: %+v", got)
				}
				if scenario == "denied-live-descendant" {
					if !got.DescendantLiveBefore {
						t.Fatal("finite descendant was not live in the owned group before cleanup")
					}
					if !got.CleanupError {
						t.Fatal("denied termination of a live descendant must return ErrCleanup")
					}
				} else if got.CleanupError {
					t.Fatal("legitimate zombie-only EPERM must not return ErrCleanup")
				}
			})
		}
	}
}
