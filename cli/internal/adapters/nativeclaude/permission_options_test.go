//go:build darwin || linux

package nativeclaude

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPermissionOptionsVersionedPreservation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unspecified", nil},
		{"default", []string{"--permission-mode", "default"}},
		{"dontAsk", []string{"--permission-mode=dontAsk"}},
		{"plan", []string{"--permission-mode", "plan"}},
		{"acceptEdits", []string{"--permission-mode", "acceptEdits"}},
		{"bypassPermissions", []string{"--permission-mode=bypassPermissions"}},
		{"auto", []string{"--permission-mode", "auto"}},
		{"manual", []string{"--permission-mode=manual"}},
		{"skip", []string{"--dangerously-skip-permissions"}},
		{"allow", []string{"--allow-dangerously-skip-permissions"}},
		{"standalone-between-values", []string{"--permission-mode", "manual", "--allow-dangerously-skip-permissions", "--settings", `{"synthetic":true}`, "--dangerously-skip-permissions", "--bare"}},
		{"supplied-order-and-duplicates", []string{"--permission-mode=auto", "--allow-dangerously-skip-permissions", "--permission-mode", "manual", "--allow-dangerously-skip-permissions"}},
	}
	for _, version := range []string{"2.1.285", "2.1.287"} {
		for _, tc := range cases {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				o := unitOptions(t, "normal")
				o.ConfigArgs = append([]string(nil), tc.args...)
				args, _, _, _, err := launchOptionsForVersion(o, version)
				path := filepath.Join(o.Cwd, suppliedTestSessionID+".jsonl")
				resume, resumeErr := (idleLaunch{version: version, model: o.Model, configArgs: tc.args}).resumeArgs(path)
				if version != "2.1.287" {
					if !errors.Is(err, ErrState) || !errors.Is(resumeErr, ErrState) {
						t.Fatal("unsupported version accepted launch or resume options", err, resumeErr)
					}
					return
				}
				if err != nil || resumeErr != nil {
					t.Fatal(err, resumeErr)
				}
				prompts := "none"
				if version == "2.1.287" {
					prompts = "host"
				}
				want := append([]string{}, tc.args...)
				want = append(want, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--await-initialize", "--permission-prompt-tool", "stdio", "--permission-prompts", prompts, "--prompt-suggestions", "false", "--replay-user-messages", "--model", o.Model)
				wantResume := append([]string{}, tc.args...)
				wantResume = append(wantResume, "--model", o.Model, "--resume", path)
				if !reflect.DeepEqual(args, want) || !reflect.DeepEqual(resume, wantResume) || !reflect.DeepEqual(o.ConfigArgs, tc.args) {
					t.Fatal("supplied options were rewritten or permissions inserted")
				}
			})
		}
	}
}

func TestPermissionOptionsRejectUnsupportedFormsAndVersions(t *testing.T) {
	invalid := [][]string{
		{"--permission-mode"}, {"--permission-mode="}, {"--permission-mode", "unknown"},
		{"--permission-mode", "accept-edits"}, {"--permission-mode", "AUTO"},
		{"--dangerously-skip-permissions=true"}, {"--dangerously-skip-permissions", "false"},
		{"--allow-dangerously-skip-permissions=false"}, {"--allow-dangerously-skip-permissions", "true"},
		{"--permission-mode", "manual\x00"}, {"--permission-mode", "manual", "-p"},
	}
	path := filepath.Join(t.TempDir(), suppliedTestSessionID+".jsonl")
	for _, version := range []string{"2.1.285", "2.1.287"} {
		for _, args := range invalid {
			if validArgs(args, version) {
				t.Fatalf("accepted malformed options for %s: %q", version, args)
			}
			if _, err := (idleLaunch{version: version, configArgs: args}).resumeArgs(path); !errors.Is(err, ErrState) {
				t.Fatal("resume accepted malformed options", err)
			}
		}
		// Resume must not reinterpret a separated option-looking value as a flag.
		if _, err := (idleLaunch{version: version, configArgs: []string{"--settings", "--dangerously-skip-permissions"}}).resumeArgs(path); !errors.Is(err, ErrState) {
			t.Fatal("resume accepted an ambiguous value", err)
		}
	}
	for _, version := range []string{"", "2.1.285", "2.1.286", "2.1.288"} {
		if validArgs(nil, version) {
			t.Fatal("unbound version accepted")
		}
		if _, err := (idleLaunch{version: version}).resumeArgs(path); !errors.Is(err, ErrState) {
			t.Fatal("resume accepted an unbound version", err)
		}
	}
}

func TestPermissionOptionsFrozenAcrossOwnedLaunchAndResume(t *testing.T) {
	f := ordinaryFixture(t, "preapproved")
	f.opts.ConfigArgs = []string{"--bare", "--permission-mode", "manual", "--allow-dangerously-skip-permissions", "--settings={\"synthetic\":true}", "--dangerously-skip-permissions"}
	wantConfig := append([]string{}, f.opts.ConfigArgs...)
	wantLaunch, _, _, _, err := launchOptionsForVersion(f.opts, "2.1.287")
	if err != nil {
		t.Fatal(err)
	}
	e := f.start(t, true)
	wantLaunch = append(wantLaunch, "--session-id", e.SessionID())
	if !reflect.DeepEqual(e.s.process.cmd.Args[1:], wantLaunch) || e.s.launch.version != e.HostVersion() {
		t.Fatal("initial process did not receive the pinned original options")
	}
	// Neither changing caller-owned slices nor mutating returned arguments may
	// change the issued launch settings or the version used to validate them.
	f.opts.ConfigArgs[2], f.opts.Model = "plan", "changed"
	resume, err := e.ResumeArguments()
	if err != nil {
		t.Fatal(err)
	}
	wantResume := append(wantConfig, "--model", "sonnet", "--resume", e.OwnedArchivePath())
	if !reflect.DeepEqual(resume, wantResume) {
		t.Fatal("resume normalized or lost original permission options")
	}
	resume[0] = "--print"
	if _, err := e.RunOrdinary(firstExchangeRunContext(t), "synthetic ordinary question", firstExchangeAllow, InteractionHandlers{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyArchive(context.Background(), e.OwnedArchivePath()); err != nil {
		t.Fatal(err)
	}
	plan, err := e.PrepareIdleResume(context.Background(), e.OwnedArchivePath())
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := plan.resumeCommand(context.Background(), strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || !reflect.DeepEqual(cmd.Args[1:], wantResume) || plan.launch.version != "2.1.287" {
		t.Fatal("verified resume lost frozen options/version", err)
	}
}

func TestPermissionOptionsMalformedRejectBeforeProbe(t *testing.T) {
	f := ordinaryFixture(t, "normal")
	f.opts.ConfigArgs = []string{"--permission-mode", "unknown", "--dangerously-skip-permissions"}
	marker := filepath.Join(f.opts.Cwd, "version-launched")
	f.opts.Env = append(f.opts.Env, "CXT_ORDINARY_VERSION_ARCHIVE="+marker)
	if _, err := StartFirstExchange(context.Background(), f.opts); !errors.Is(err, ErrState) {
		t.Fatal("malformed permission options accepted", err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("option rejection launched a probe")
	}
	if _, err := os.Lstat(f.recorder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("option rejection launched the protocol process")
	}
}

func TestPermissionOptionsRejectMismatchedFrozenResumeVersion(t *testing.T) {
	o, _ := idleUnitOptions(t, "exit")
	s := retiredIdleSession(t, o)
	if s.s.launch.version != "2.1.287" {
		t.Fatal("ordinary launch version was not frozen")
	}
	s.s.launch.version = "2.1.285"
	if _, err := s.PrepareIdleResume(context.Background(), unitArchive(s.s)); !errors.Is(err, ErrState) {
		t.Fatal("resume accepted a different permission-validation version", err)
	}
}
