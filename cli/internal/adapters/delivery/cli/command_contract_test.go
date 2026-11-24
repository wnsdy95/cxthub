package cli

import (
	"reflect"
	"strings"
	"testing"
)

const contractSession = "11111111-1111-4111-8111-111111111111"

func TestCommandContractRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"repo", "delete", "https://example.test/a/b"}, {"repo", "create"},
		{"init", "ignored-url"}, {"config"},
		{"config", "segments.auto", "off", "extra"}, {"config", "load.mode"},
		{"login", "literal-token", "-t", "another-token"},
		{"remote", "add", "origin"}, {"remote", "rm", "a", "b"},
		{"remote", "typo"}, {"remote", "set-endpoint", "origin", "https://example.test/api/v1", "--migrate-pending"},
		{"settings", "restore", "not-an-index"},
		{"settings", "restore", "-1"}, {"settings", "pull", "extra"},
		{"secrets", "push", "--force"}, {"secrets", "typo"},
		{"hooks", "install", "extra"},
		{"hook", "--provider", "codex"}, {"hook", "--event", "stop"},
		{"version", "extra"},
		{"login", ""}, {"remote", "add", "origin", ""},
		// A session is named by its tool and its identifier.
		{"segments", "push", "claude"}, {"push", "claude"}, {"push", "other", contractSession},
		{"remove", "claude", contractSession, "--at", "abc"}, {"remove"},
		{"allow"}, {"resend"}, {"archive", "claude"}, {"archive", "claude", contractSession, "--json"},
		{"doctor", "extra"}, {"refresh", "--json"}, {"refresh", "extra"},
		{"fork"}, {"fork", "--at", "abc"}, {"fork", "--branch", "topic"},
		{"fork", "claude", contractSession, "--to", "other"}, {"fork", "claude", contractSession, "--branch", "topic"},
		{"push", "--yes"}, {"segments", "typo"},
		// Commands of the removed capture approach are not commands any more.
		{"save"}, {"status"}, {"checkout", "main"}, {"pull"}, {"commit", "-m", "x"}, {"stash"}, {"load"}, {"claude"}, {"codex"},
		{"mcp", "--local"}, {"capture", "list"}, {"sync", "status"}, {"branch"}, {"tag"}, {"fsck"}, {"repair", "--from-server"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(append([]string{"cxt"}, args...)); handled || err == nil {
				t.Fatalf("invalid invocation accepted: handled=%v error=%v", handled, err)
			}
			// A nil container proves dispatch rejects the invocation before
			// touching any command service, not merely in the process wrapper.
			if err := Run(nil, append([]string{"cxt"}, args...)); err == nil {
				t.Fatal("invalid invocation reached dispatch")
			}
		})
	}
}

func TestTypedArgumentsPreserveLiteralValues(t *testing.T) {
	p, help, err := parseCommand("login", []string{"--server=https://example.test", "--", "--literal-token"})
	if err != nil || help || p.first() != "--literal-token" || p.flags["--server"] != "https://example.test" {
		t.Fatalf("literal boundary: %+v %v %v", p, help, err)
	}
	p, help, err = parseCommand("secrets", []string{"pull", "-p=--help"})
	if err != nil || help || p.flags["-p"] != "--help" {
		t.Fatalf("literal help: %+v %v %v", p, help, err)
	}
	if _, _, err := parseCommand("secrets", []string{"pull", "--typo=private-passphrase"}); err == nil || strings.Contains(err.Error(), "private-passphrase") {
		t.Fatalf("argument error leaked a value: %v", err)
	}
}

func TestCommandContractAcceptsSupportedArguments(t *testing.T) {
	cases := [][]string{
		{"secrets", "pull", "--force", "-p=-passphrase"},
		{"secrets", "push", "--rotate", "--remember"},
		{"remote", "add", "origin", "https://example.test/a/b"},
		{"remote", "rm", "origin"}, {"remote", "-v"},
		{"remote", "set-endpoint", "origin", "https://example.test/api/v1"},
		{"settings", "restore", "0"}, {"settings", "restore"},
		{"hook", "--provider", "codex", "--event", "stop"},
		{"config", "secrets.redact", ""}, {"config", "segments.auto"}, {"config", "capture.identity", "default"},
		{"setup"}, {"setup", "https://example.test/a/b", "--no-login"}, {"init", "--no-hooks"},
		{"setup", "--git-remote", "upstream", "https://example.test/a/b"}, {"setup", "--git-remote=upstream", "--no-login"},
		{"segments"}, {"segments", "--json"},
		{"push"}, {"push", "--json"}, {"push", "codex", contractSession},
		{"log"}, {"log", "claude", contractSession, "--json"},
		{"remove", "claude", contractSession, "--yes"}, {"allow", "claude", contractSession},
		{"resend", "codex", contractSession}, {"archive", "claude", contractSession}, {"unarchive", "claude", contractSession},
		{"doctor"}, {"doctor", "--json"}, {"refresh"},
		{"fork", "--at", "abc", "--branch", "topic"},
		{"fork", "claude", contractSession}, {"fork", "claude", contractSession, "--at", "abc", "--to", "codex", "--branch", "topic", "--json"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(append([]string{"cxt"}, args...)); handled || err != nil {
				t.Fatalf("valid invocation rejected: handled=%v error=%v", handled, err)
			}
		})
	}
}

// Every session command is the same at the top level and under `segments`.
func TestSessionCommandsAreTheSameAtTheTopLevel(t *testing.T) {
	for verb := range segmentVerbs {
		short := CanonicalArgs([]string{"cxt", verb, "claude", contractSession, "--json"})
		long := CanonicalArgs([]string{"cxt", "segments", verb, "claude", contractSession, "--json"})
		if !reflect.DeepEqual(short, long) {
			t.Fatalf("%s: %v and %v", verb, short, long)
		}
	}
	// Other commands, and an invocation with no command, are left as they are.
	for _, args := range [][]string{{"cxt"}, {"cxt", "setup"}, {"cxt", "segments", "push"}, {"cxt", "git-hook", "push"}} {
		if got := CanonicalArgs(args); !reflect.DeepEqual(got, args) {
			t.Fatalf("%v became %v", args, got)
		}
	}
	// The unknown-command message offers both spellings' names.
	if err := unknownCommandError("typo"); !strings.Contains(err.Error(), "doctor") || !strings.Contains(err.Error(), "segments") {
		t.Fatalf("unknown command: %v", err)
	}
}
