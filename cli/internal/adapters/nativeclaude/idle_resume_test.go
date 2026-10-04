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
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This helper is selected only in subprocesses with a synthetic environment.
// Reuse the original no-turn protocol fixture; the resume branch is a plain
// terminal process and accepts no model requests or credentials.
func init() {
	if os.Getenv("CXT_CLAUDE_IDLE_HELPER") == "1" {
		idleUnitHelper()
		os.Exit(0)
	}
}

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
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	cwd, _ := os.Getwd()
	log, err := os.OpenFile(os.Getenv("CXT_CLAUDE_IDLE_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(9)
	}
	_ = json.NewEncoder(log).Encode(idleInvocation{Args: os.Args[1:], Cwd: cwd, Env: os.Environ()})
	_ = log.Close()
	_, _ = fmt.Fprintln(os.Stdout, "fixture process started")
	switch mode {
	case "wait", "ignore-term":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "fail":
		_, _ = fmt.Fprintln(os.Stderr, "PRIVATE_NATIVE_DIAGNOSTIC")
		os.Exit(17)
	}
}

func idleUnitOptions(t *testing.T, mode string) (Options, string) {
	t.Helper()
	opts := unitOptions(t, "normal")
	log := filepath.Join(opts.Cwd, "idle-invocations.jsonl")
	opts.ConfigArgs = []string{"--bare", "--settings", `{"synthetic":true}`, "--setting-sources=", "--tools", "", "--permission-mode", "default"}
	opts.Env = append(opts.Env, "CXT_CLAUDE_IDLE_HELPER=1", "CXT_CLAUDE_IDLE_MODE="+mode, "CXT_CLAUDE_IDLE_LOG="+log, "CXT_IDLE_SENTINEL=original")
	return opts, log
}

func retiredIdleSession(t *testing.T, opts Options) *Session {
	t.Helper()
	s, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.AppendReference(context.Background(), "synthetic idle reference"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyArchive(context.Background(), unitArchive(s)); err != nil {
		t.Fatal(err)
	}
	return s
}

func idleUnitPlan(t *testing.T, opts Options) (*Session, *IdleResumePlan) {
	t.Helper()
	s := retiredIdleSession(t, opts)
	plan, err := s.PrepareIdleResume(context.Background(), unitArchive(s))
	if err != nil {
		t.Fatal(err)
	}
	return s, plan
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

func assertIdleReaped(t *testing.T, p *IdleProcess) {
	t.Helper()
	select {
	case <-p.Done():
	default:
		t.Fatal("lifecycle completed before cleanup")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("process was never reaped")
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(p.cmd.Process.Pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatal("owned process remains waitable", err)
	}
}

func TestIdleResumePreservesFrozenInvocationAndCallerFiles(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	wantArgs := append([]string{}, opts.ConfigArgs...)
	wantEnv := append([]string{}, opts.Env...)
	s := retiredIdleSession(t, opts)
	// The original slices must not remain an authority after Start returns.
	opts.ConfigArgs[0], opts.Env[0], opts.Model, opts.Cwd, opts.Executable = "--print", "HOME=/changed", "changed", "/changed", "/changed"
	t.Setenv("CXT_IDLE_SENTINEL", "ambient changed")
	plan, err := s.PrepareIdleResume(context.Background(), unitArchive(s))
	if err != nil {
		t.Fatal(err)
	}
	in, out, diagnostic := idleFiles(t)
	p, err := plan.Start(context.Background(), in, out, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got := awaitIdleInvocation(t, log)
	wantArgs = append(wantArgs, "--model", "sonnet", "--resume", unitArchive(s))
	if !reflect.DeepEqual(got.Args, wantArgs) || !reflect.DeepEqual(got.Env, wantEnv) || got.Cwd != s.cwd {
		t.Fatal("resume changed the issued invocation or inherited later environment")
	}
	for _, f := range []*os.File{in, out, diagnostic} {
		if _, err := f.Stat(); err != nil {
			t.Fatal("library closed caller-owned file")
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	assertIdleReaped(t, p)
	if _, err := plan.Start(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
		t.Fatal("plan restarted a finished process", err)
	}
	if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s)); !errors.Is(err, ErrState) {
		t.Fatal("session issued a replacement plan", err)
	}
	if _, err := os.Stat(unitArchive(s)); err != nil {
		t.Fatal("resume removed archive", err)
	}
}

func TestIdleResumeRequiresSuccessfulRetirementAndArchiveVerification(t *testing.T) {
	for _, mode := range []string{"normal", "late-assistant"} {
		t.Run(mode, func(t *testing.T) {
			s := unitSession(t, mode)
			path := unitArchive(s)
			if _, err := s.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("active session produced resume plan", err)
			}
			if _, err := s.AppendReference(context.Background(), "synthetic"); err != nil {
				t.Fatal(err)
			}
			err := s.Close()
			if mode == "normal" && err != nil || mode != "normal" && err == nil {
				t.Fatal("unexpected retirement result", err)
			}
			if _, err := s.PrepareIdleResume(context.Background(), path); !errors.Is(err, ErrState) {
				t.Fatal("unverified or failed retirement produced plan", err)
			}
			if mode != "normal" {
				if _, err := s.VerifyArchive(context.Background(), path); err == nil {
					t.Fatal("failed retirement verified")
				}
				return
			}
			if _, err := s.VerifyArchive(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PrepareIdleResume(context.Background(), filepath.Join(filepath.Dir(path), "foreign.jsonl")); !errors.Is(err, ErrState) {
				t.Fatal("foreign archive accepted", err)
			}
			if _, err := s.PrepareIdleResume(context.Background(), path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdleResumeRejectsRawArchiveDriftAndSymlinks(t *testing.T) {
	for _, mode := range []string{"metadata", "metadata same stat", "replace same bytes", "file symlink", "parent symlink", "oversized", "fifo", "before prepare"} {
		t.Run(mode, func(t *testing.T) {
			opts, log := idleUnitOptions(t, "exit")
			s := retiredIdleSession(t, opts)
			path := unitArchive(s)
			if mode == "metadata same stat" {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("{\"type\":\"unknown_metadata\",\"value\":1}\n")
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
				changed := bytes.Replace(raw, []byte(`"value":1`), []byte(`"value":2`), 1)
				if bytes.Equal(raw, changed) {
					t.Fatal("metadata fixture did not change")
				}
				if err = os.WriteFile(path, changed, 0600); err == nil {
					err = os.Chtimes(path, info.ModTime(), info.ModTime())
				}
				if err == nil && validateIdlePath(plan.archivePath) != nil {
					t.Fatal("fixture must pass stat checks so raw fingerprint is decisive")
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
				if _, err := plan.Start(context.Background(), in, out, diagnostic); err == nil || time.Since(started) > time.Second {
					t.Fatal("changed/special archive launched or blocked", err)
				}
				if _, err := plan.Start(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
					t.Fatal("failed plan was reused", err)
				}
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("resume launched after archive drift")
			}
		})
	}
}

func TestIdleResumeCancellationAndConcurrentCloseReap(t *testing.T) {
	for _, mode := range []string{"lifetime", "wait", "close", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			helperMode := "wait"
			if mode == "ignore-term" {
				helperMode = mode
			}
			opts, log := idleUnitOptions(t, helperMode)
			_, plan := idleUnitPlan(t, opts)
			in, out, diagnostic := idleFiles(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, err := plan.Start(ctx, in, out, diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			awaitIdleInvocation(t, log)
			started := time.Now()
			switch mode {
			case "lifetime", "ignore-term":
				cancel()
				if err := p.Wait(context.Background()); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
					t.Fatal("lifetime cancellation was lost or not reaped", err)
				}
			case "wait":
				wait, stop := context.WithCancel(context.Background())
				stop()
				if err := p.Wait(wait); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
					t.Fatal("wait cancellation detached the process", err)
				}
			case "close":
				var wg sync.WaitGroup
				errors := make(chan error, 4)
				copy := *p
				for i := 0; i < 4; i++ {
					target := p
					if i%2 == 0 {
						target = &copy
					}
					wg.Go(func() { errors <- target.Close() })
				}
				wg.Wait()
				close(errors)
				for err := range errors {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("cleanup was not bounded")
			}
			assertIdleReaped(t, p)
		})
	}
}

func TestIdleResumeFailedLaunchCancellationAndExitNeverReplace(t *testing.T) {
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
			p, err := plan.Start(ctx, in, out, diagnostic)
			if p != nil {
				t.Cleanup(func() { _ = p.Close() })
			}
			if mode == "nonzero" {
				if err != nil {
					t.Fatal(err)
				}
				err = p.Wait(context.Background())
				if !errors.Is(err, ErrIdleExit) || strings.Contains(fmt.Sprint(err), "PRIVATE") {
					t.Fatal("nonzero exit ignored or diagnostic leaked", err)
				}
				assertIdleReaped(t, p)
				awaitIdleInvocation(t, log)
			} else {
				if err == nil || p != nil {
					t.Fatal("failed launch returned a process")
				}
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatal("failed/cancelled launch ran a process")
				}
			}
			if _, err := plan.Start(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
				t.Fatal("failed plan retried", err)
			}
			if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s)); !errors.Is(err, ErrState) {
				t.Fatal("failed plan replaced", err)
			}
		})
	}
}

func TestIdleResumeRejectsExecutableAndCwdDrift(t *testing.T) {
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
			if _, err := plan.Start(context.Background(), in, out, diagnostic); !errors.Is(err, ErrState) {
				t.Fatal("changed launch identity was accepted", err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("changed launch identity ran a process")
			}
		})
	}
}

func TestIdleResumeRetainsResolvedExecutableAcrossAliasChange(t *testing.T) {
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
	p, err := plan.Start(context.Background(), in, out, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitIdleInvocation(t, log)
	assertIdleReaped(t, p)
}

func TestIdleResumeConcurrentStartIsOneShot(t *testing.T) {
	opts, log := idleUnitOptions(t, "exit")
	_, plan := idleUnitPlan(t, opts)
	in, out, diagnostic := idleFiles(t)
	type result struct {
		process *IdleProcess
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
			p, err := target.Start(context.Background(), in, out, diagnostic)
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
		if err := result.process.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertIdleReaped(t, result.process)
	}
	if started != 1 {
		t.Fatal("concurrent Start did not issue exactly one process")
	}
	awaitIdleInvocation(t, log) // Decoding also rejects multiple invocation rows.
}

func TestIdleResumeArgumentsRequireExactArchiveAndOriginalAllowedOptions(t *testing.T) {
	id := "12345678-1234-4234-8234-123456789abc"
	path := filepath.Join(t.TempDir(), id+".jsonl")
	for _, bad := range []string{id, id + ".jsonl", filepath.Join(t.TempDir(), "other.jsonl"), path + "/../" + id + ".jsonl", path + "\x00", strings.TrimSuffix(path, ".jsonl")} {
		if _, err := (idleLaunch{}).resumeArgs(bad); !errors.Is(err, ErrState) {
			t.Fatal("non-exact archive target accepted")
		}
	}
	for _, args := range [][]string{{"-p"}, {"--resume", path}, {"--session-id", id}, {"--output-format", "stream-json"}, {"--settings", "--print"}, {"--permission-mode", "bypassPermissions"}, {"a prompt"}} {
		if _, err := (idleLaunch{configArgs: args}).resumeArgs(path); !errors.Is(err, ErrState) {
			t.Fatal("unsupported/helper options accepted")
		}
	}
	if _, err := (idleLaunch{model: "--print"}).resumeArgs(path); !errors.Is(err, ErrState) {
		t.Fatal("option-looking model accepted")
	}
	opts, _ := idleUnitOptions(t, "exit")
	opts.ConfigArgs = []string{"--settings", "--print"}
	s := retiredIdleSession(t, opts) // Legacy no-turn allowance is intentionally unchanged.
	if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s)); !errors.Is(err, ErrState) {
		t.Fatal("ambiguous original options produced idle plan", err)
	}
}
