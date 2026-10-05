//go:build darwin || linux

package nativeclaude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This synthetic terminal is selected by the ordinary fixture's --resume
// branch. Its caller exclusively owns termination and Wait, like the CLI supervisor.
type idleInvocation struct {
	Args []string
	Cwd  string
	Env  []string
}

func idleUnitHelper() {
	resume := false
	for _, arg := range os.Args[1:] {
		resume = resume || arg == "--resume"
	}
	if !resume {
		claudeUnitHelper()
		return
	}
	mode := os.Getenv("CXT_CLAUDE_IDLE_MODE")
	cwd, _ := os.Getwd()
	log, err := os.OpenFile(os.Getenv("CXT_CLAUDE_IDLE_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(9)
	}
	_ = json.NewEncoder(log).Encode(idleInvocation{Args: os.Args[1:], Cwd: cwd, Env: os.Environ()})
	_ = log.Close()
	_, _ = fmt.Fprintln(os.Stdout, "fixture process started")
	switch mode {
	case "wait":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "fail":
		_, _ = fmt.Fprintln(os.Stderr, "PRIVATE_NATIVE_DIAGNOSTIC")
		os.Exit(17)
	}
}

func idleUnitOptions(t *testing.T, mode string) (Options, string) {
	t.Helper()
	opts := newFirstExchangeFixture(t, "normal").opts
	log := filepath.Join(opts.Cwd, "idle-invocations.jsonl")
	opts.ConfigArgs = []string{"--bare", "--settings", `{"synthetic":true}`, "--setting-sources=", "--tools", "", "--permission-mode", "default"}
	opts.Env = append(opts.Env, "CXT_CLAUDE_IDLE_MODE="+mode, "CXT_CLAUDE_IDLE_LOG="+log, "CXT_IDLE_SENTINEL=original")
	return opts, log
}

func retiredIdleSession(t *testing.T, opts Options) *FirstExchange {
	t.Helper()
	e := (firstExchangeFixture{opts: opts}).start(t, true)
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "synthetic question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	completedExchangeRows(t, e, "normal")
	if _, err := e.VerifyArchive(context.Background(), unitArchive(e.s)); err != nil {
		t.Fatal(err)
	}
	return e
}

func idleUnitPlan(t *testing.T, opts Options) (*FirstExchange, *IdleResumePlan) {
	t.Helper()
	e := retiredIdleSession(t, opts)
	plan, err := e.PrepareIdleResume(context.Background(), unitArchive(e.s))
	if err != nil {
		t.Fatal(err)
	}
	return e, plan
}

// Wait remains test-supervisor-owned, with a finite guard for broken fixtures.
func waitSupervised(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("supervised helper could not be reaped")
		}
		t.Fatal("supervised helper exceeded its bounded wait")
		return ErrCleanup
	}
}

func idleFiles(t *testing.T) (stdin, stdout, stderr *os.File) {
	t.Helper()
	var err error
	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = writer.Close() })
	stdout, err = os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err = os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdout.Close(); _ = stderr.Close() })
	return stdin, stdout, stderr
}

func awaitIdleInvocation(t *testing.T, path string) idleInvocation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		var got idleInvocation
		if err == nil && json.Unmarshal(raw, &got) == nil {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fixture did not record one invocation")
	return idleInvocation{}
}

func assertSupervisedReaped(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.ProcessState == nil {
		t.Fatal("supervisor never reaped its process")
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatal("supervisor-owned process remains waitable", err)
	}
}

func TestSupervisedResumePreservesFrozenInvocationAndCallerFiles(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	wantArgs := append([]string{}, opts.ConfigArgs...)
	wantEnv := append([]string{}, opts.Env...)
	s := retiredIdleSession(t, opts)
	// The original slices must not remain an authority after Start returns.
	opts.ConfigArgs[0], opts.Env[0], opts.Model, opts.Cwd, opts.Executable = "--print", "HOME=/changed", "changed", "/changed", "/changed"
	t.Setenv("CXT_IDLE_SENTINEL", "ambient changed")
	plan, err := s.PrepareIdleResume(context.Background(), unitArchive(s.s))
	if err != nil {
		t.Fatal(err)
	}
	in, out, diagnostic := idleFiles(t)
	p, err := plan.StartSupervised(context.Background(), in, out, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitSupervised(t, p); err != nil {
		t.Fatal(err)
	}
	got := awaitIdleInvocation(t, log)
	wantArgs = append(wantArgs, "--model", "sonnet", "--resume", unitArchive(s.s))
	if !reflect.DeepEqual(got.Args, wantArgs) || !reflect.DeepEqual(got.Env, wantEnv) || got.Cwd != s.s.cwd {
		t.Fatal("resume changed the issued invocation or inherited later environment")
	}
	for _, f := range []*os.File{in, out, diagnostic} {
		if _, err := f.Stat(); err != nil {
			t.Fatal("library closed caller-owned file")
		}
	}
	assertSupervisedReaped(t, p)
	if _, err := plan.StartSupervised(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
		t.Fatal("plan restarted a finished process", err)
	}
	if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s.s)); !errors.Is(err, ErrState) {
		t.Fatal("session issued a replacement plan", err)
	}
	if _, err := os.Stat(unitArchive(s.s)); err != nil {
		t.Fatal("resume removed archive", err)
	}
}

func TestSupervisedResumeRequiresSuccessfulRetirementAndArchiveVerification(t *testing.T) {
	for _, mode := range []string{"normal", "late-assistant"} {
		t.Run(mode, func(t *testing.T) {
			e := newFirstExchangeFixture(t, mode).start(t, true)
			path := unitArchive(e.s)
			if _, err := e.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("active session produced resume plan", err)
			}
			_, err := e.RunOrdinary(firstExchangeRunContext(t), "synthetic question", firstExchangeAllow, InteractionHandlers{})
			if mode == "normal" && err != nil || mode != "normal" && err == nil {
				t.Fatal("unexpected retirement result", err)
			}
			if _, err := e.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("unverified or failed retirement produced plan", err)
			}
			if mode != "normal" {
				if _, err := e.VerifyArchive(context.Background(), path); !errors.Is(err, ErrState) {
					t.Fatal("failed retirement verified", err)
				}
				return
			}
			completedExchangeRows(t, e, mode)
			if _, err := e.VerifyArchive(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := e.PrepareIdleResume(context.Background(), filepath.Join(filepath.Dir(path), "foreign.jsonl")); !errors.Is(err, ErrState) {
				t.Fatal("foreign archive accepted", err)
			}
			if _, err := e.PrepareIdleResume(context.Background(), path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSupervisedResumeRejectsRawArchiveDriftAndSymlinks(t *testing.T) {
	for _, mode := range []string{"metadata", "metadata same stat", "replace same bytes", "file symlink", "parent symlink", "oversized", "fifo", "before prepare"} {
		t.Run(mode, func(t *testing.T) {
			opts, log := idleUnitOptions(t, "exit")
			s := retiredIdleSession(t, opts)
			path := unitArchive(s.s)
			if mode == "metadata same stat" {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(fmt.Sprintf("{\"type\":\"last-prompt\",\"sessionId\":%q,\"lastPrompt\":\"one\"}\n", s.SessionID()))
				_ = file.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.VerifyArchive(context.Background(), path); err != nil {
					t.Fatal(err)
				}
			}
			var plan *IdleResumePlan
			var err error
			if mode != "before prepare" {
				plan, err = s.PrepareIdleResume(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "metadata", "before prepare":
				// The reference remains byte-exact; an unknown metadata row changes.
				err = os.WriteFile(path, append(raw, []byte("{\"type\":\"unknown_metadata\",\"value\":1}\n")...), 0600)
			case "metadata same stat":
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				changed := bytes.Replace(raw, []byte(`"lastPrompt":"one"`), []byte(`"lastPrompt":"two"`), 1)
				if bytes.Equal(raw, changed) {
					t.Fatal("metadata fixture did not change")
				}
				if err = os.WriteFile(path, changed, 0600); err == nil {
					err = os.Chtimes(path, info.ModTime(), info.ModTime())
				}
				if err == nil && validateIdlePath(plan.archivePath) != nil {
					t.Fatal("fixture must pass stat checks so raw fingerprint is decisive")
				}
				if err == nil {
					if _, verifyErr := s.s.verifyExchangeArchive(context.Background(), path); verifyErr != nil {
						t.Fatal("changed display metadata must remain semantically valid so raw digest is decisive", verifyErr)
					}
				}
			case "replace same bytes", "file symlink":
				replacement := path + ".replacement"
				if err = os.WriteFile(replacement, raw, 0600); err == nil {
					err = os.Remove(path)
				}
				if err == nil && mode == "file symlink" {
					err = os.Symlink(replacement, path)
				} else if err == nil {
					err = os.Rename(replacement, path)
				}
			case "parent symlink":
				dir := filepath.Dir(path)
				if err = os.Rename(dir, dir+"-moved"); err == nil {
					err = os.Symlink(dir+"-moved", dir)
				}
			case "oversized":
				err = os.Truncate(path, (64<<20)+1)
			case "fifo":
				if err = os.Remove(path); err == nil {
					err = syscall.Mkfifo(path, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "before prepare" {
				if _, err := s.PrepareIdleResume(context.Background(), path); err == nil {
					t.Fatal("archive drift after verification rebound silently")
				}
			} else {
				in, out, diagnostic := idleFiles(t)
				started := time.Now()
				if _, err := plan.StartSupervised(context.Background(), in, out, diagnostic); err == nil || time.Since(started) > time.Second {
					t.Fatal("changed/special archive launched or blocked", err)
				}
				if _, err := plan.StartSupervised(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
					t.Fatal("failed plan was reused", err)
				}
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("resume launched after archive drift")
			}
		})
	}
}

func TestSupervisedResumeTransfersLifetimeOwnership(t *testing.T) {
	opts, log := idleUnitOptions(t, "wait")
	_, plan := idleUnitPlan(t, opts)
	in, out, diagnostic := idleFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := plan.StartSupervised(ctx, in, out, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = waitSupervised(t, cmd)
		}
	}()
	awaitIdleInvocation(t, log)
	if cmd.SysProcAttr != nil {
		_ = cmd.Process.Kill()
		_ = waitSupervised(t, cmd)
		t.Fatal("supervised TUI acquired isolated process group")
	}
	cancel()
	// Cancellation of the launch context must not install a second process
	// owner. The actual supervisor kills/reaps this returned command.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("launch cancellation retired supervisor-owned process", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := waitSupervised(t, cmd); !errors.As(err, &exit) {
		t.Fatal("supervisor did not observe termination", err)
	}
	assertSupervisedReaped(t, cmd)
	for _, file := range []*os.File{in, out, diagnostic} {
		if _, err := file.Stat(); err != nil {
			t.Fatal("caller file closed", err)
		}
	}
}

func TestSupervisedResumeFailedLaunchCancellationAndExitNeverReplace(t *testing.T) {
	for _, mode := range []string{"cancelled", "closed file", "nonzero"} {
		t.Run(mode, func(t *testing.T) {
			opts, log := idleUnitOptions(t, "fail")
			s, plan := idleUnitPlan(t, opts)
			in, out, diagnostic := idleFiles(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			if mode == "closed file" {
				_ = out.Close()
			}
			p, err := plan.StartSupervised(ctx, in, out, diagnostic)
			if mode == "nonzero" {
				if err != nil {
					t.Fatal(err)
				}
				err = waitSupervised(t, p)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 17 || strings.Contains(fmt.Sprint(err), "PRIVATE") {
					t.Fatal("nonzero exit ignored or diagnostic leaked", err)
				}
				assertSupervisedReaped(t, p)
				awaitIdleInvocation(t, log)
			} else {
				if err == nil || p != nil {
					t.Fatal("failed launch returned a process")
				}
				if mode == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatal("launch cancellation lost its cause", err)
				}
				if mode == "closed file" && !errors.Is(err, ErrState) {
					t.Fatal("closed caller file returned wrong error", err)
				}
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatal("failed/cancelled launch ran a process")
				}
			}
			if _, err := plan.StartSupervised(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
				t.Fatal("failed plan retried", err)
			}
			if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s.s)); !errors.Is(err, ErrState) {
				t.Fatal("failed plan replaced", err)
			}
		})
	}
}

func TestSupervisedResumeRejectsExecutableAndCwdDrift(t *testing.T) {
	for _, mode := range []string{"executable content", "executable replacement", "executable symlink", "cwd replacement"} {
		t.Run(mode, func(t *testing.T) {
			opts, log := idleUnitOptions(t, "exit")
			wrapper := filepath.Join(opts.Cwd, "owned-launcher")
			body := []byte("#!/bin/sh\nexec '" + strings.ReplaceAll(opts.Executable, "'", "'\"'\"'") + "' \"$@\"\n")
			if err := os.WriteFile(wrapper, body, 0700); err != nil {
				t.Fatal(err)
			}
			opts.Executable, opts.Cwd = wrapper, filepath.Join(opts.Cwd, "work")
			if err := os.Mkdir(opts.Cwd, 0700); err != nil {
				t.Fatal(err)
			}
			_, plan := idleUnitPlan(t, opts)
			var err error
			switch mode {
			case "executable content":
				err = os.WriteFile(wrapper, append(body, []byte("# changed\n")...), 0700)
			case "executable replacement":
				if err = os.WriteFile(wrapper+".new", body, 0700); err == nil {
					err = os.Rename(wrapper+".new", wrapper)
				}
			case "executable symlink":
				if err = os.Rename(wrapper, wrapper+".old"); err == nil {
					err = os.Symlink(wrapper+".old", wrapper)
				}
			case "cwd replacement":
				if err = os.Rename(opts.Cwd, opts.Cwd+".old"); err == nil {
					err = os.Mkdir(opts.Cwd, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			in, out, diagnostic := idleFiles(t)
			if _, err := plan.StartSupervised(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
				t.Fatal("changed launch identity was accepted", err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("changed launch identity ran a process")
			}
		})
	}
}

func TestSupervisedResumeRetainsResolvedExecutableAcrossAliasChange(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	alias := filepath.Join(opts.Cwd, "alias")
	if err := os.Symlink(opts.Executable, alias); err != nil {
		t.Fatal(err)
	}
	opts.Executable = alias
	_, plan := idleUnitPlan(t, opts)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(opts.Cwd, "missing replacement"), alias); err != nil {
		t.Fatal(err)
	}
	in, out, diagnostic := idleFiles(t)
	p, err := plan.StartSupervised(context.Background(), in, out, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitSupervised(t, p); err != nil {
		t.Fatal(err)
	}
	awaitIdleInvocation(t, log)
	assertSupervisedReaped(t, p)
}

func TestSupervisedResumeConcurrentStartIsOneShot(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	_, plan := idleUnitPlan(t, opts)
	in, out, diagnostic := idleFiles(t)
	type result struct {
		process *exec.Cmd
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	copy := *plan
	for i := 0; i < 2; i++ {
		target := plan
		if i == 1 {
			target = &copy
		}
		wg.Go(func() {
			p, err := target.StartSupervised(context.Background(), in, out, diagnostic)
			results <- result{p, err}
		})
	}
	wg.Wait()
	close(results)
	started := 0
	for result := range results {
		if result.err != nil {
			if !errors.Is(result.err, ErrState) {
				t.Fatal(result.err)
			}
			continue
		}
		started++
		if err := waitSupervised(t, result.process); err != nil {
			t.Fatal(err)
		}
		assertSupervisedReaped(t, result.process)
	}
	if started != 1 {
		t.Fatal("concurrent Start did not issue exactly one process")
	}
	awaitIdleInvocation(t, log) // Decoding also rejects multiple invocation rows.
}

func TestSupervisedResumeArgumentsRequireExactArchiveAndOriginalAllowedOptions(t *testing.T) {
	id := "12345678-1234-4234-8234-123456789abc"
	path := filepath.Join(t.TempDir(), id+".jsonl")
	for _, bad := range []string{id, id + ".jsonl", filepath.Join(t.TempDir(), "other.jsonl"), path + "/../" + id + ".jsonl", path + "\x00", strings.TrimSuffix(path, ".jsonl")} {
		if _, err := (idleLaunch{version: "2.1.287"}).resumeArgs(bad); !errors.Is(err, ErrState) {
			t.Fatal("non-exact archive target accepted")
		}
	}
	for _, args := range [][]string{{"-p"}, {"--resume", path}, {"--session-id", id}, {"--output-format", "stream-json"}, {"--settings", "--print"}, {"--permission-mode", "unknown"}, {"a prompt"}} {
		if _, err := (idleLaunch{version: "2.1.287", configArgs: args}).resumeArgs(path); !errors.Is(err, ErrState) {
			t.Fatal("unsupported/helper options accepted")
		}
	}
	if _, err := (idleLaunch{version: "2.1.287", model: "--print"}).resumeArgs(path); !errors.Is(err, ErrState) {
		t.Fatal("option-looking model accepted")
	}
	opts, _ := idleUnitOptions(t, "exit")
	opts.ConfigArgs = []string{"--settings", "--print"}
	s := retiredIdleSession(t, opts) // Initial transport may accept a separated value; resume must reject ambiguity.
	if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s.s)); !errors.Is(err, ErrState) {
		t.Fatal("ambiguous original options produced idle plan", err)
	}
}
