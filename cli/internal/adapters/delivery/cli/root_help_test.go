package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/testutil"
)

func TestUsageCoversEveryPublicCommand(t *testing.T) {
	for _, command := range publicCommandNames {
		if !strings.Contains(usageText, command) {
			t.Errorf("usage text does not mention public command %q", command)
		}
	}

	if strings.Contains(usageText, "git-hook") {
		t.Error("internal git-hook command must not appear in public usage")
	}
}

func TestGlobalHelpAliasesReturnSuccessWithoutContainer(t *testing.T) {
	for _, command := range []string{"-h", "--help", "help"} {
		t.Run(command, func(t *testing.T) {
			if err := Run(nil, []string{"cxt", command}); err != nil {
				t.Fatalf("Run(%q) returned error: %v", command, err)
			}
		})
	}
}

func TestEveryPublicSubcommandHelpReturnsBeforeContainer(t *testing.T) {
	for _, command := range publicCommandNames {
		if command == "help" {
			continue
		}
		for _, help := range []string{"-h", "--help"} {
			t.Run(command+"/"+help, func(t *testing.T) {
				// A nil container is deliberate: any command dispatch would panic.
				if err := Run(nil, []string{"cxt", command, help}); err != nil {
					t.Fatalf("Run(%q, %q) returned error: %v", command, help, err)
				}
			})
		}
	}
}

func TestEveryPublicCommandHasAnArgumentSpec(t *testing.T) {
	for _, command := range publicCommandNames {
		if command == "help" {
			continue
		}
		// A session command at the top level is parsed as `segments <command>`.
		if spec, ok := commandArgSpecs[CanonicalArgs([]string{"cxt", command})[1]]; !ok || spec.usage == "" {
			t.Errorf("public command %q has no argument/usage spec", command)
		}
	}
}

func TestUnknownFlagsAreRejectedBeforeCommandDispatch(t *testing.T) {
	for command := range commandArgSpecs {
		t.Run(command, func(t *testing.T) {
			err := Run(nil, []string{"cxt", command, "--definitely-unknown"})
			if err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Fatalf("unknown flag error = %v", err)
			}
		})
	}
}

func TestValueFlagsRejectMissingValues(t *testing.T) {
	tests := [][]string{
		{"cxt", "init", "--remote"},
		{"cxt", "login", "--server"},
		{"cxt", "fork", "--at"},
		{"cxt", "segments", "fork", "--branch"},
		{"cxt", "secrets", "pull", "-p"},
		{"cxt", "hook", "--event"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[1:], "/"), func(t *testing.T) {
			err := Run(nil, args)
			if err == nil || !strings.Contains(err.Error(), "requires a value") {
				t.Fatalf("missing-value error = %v", err)
			}
		})
	}
}

func TestInternalGitHookKeepsEventSpecificArgumentOwnership(t *testing.T) {
	handled, err := PreflightArgs([]string{"cxt", "git-hook", "post-commit", "--resolve", "session-id"})
	if err != nil || handled {
		t.Fatalf("internal git-hook preflight = handled %v, err %v", handled, err)
	}
}

func TestBooleanFlagsDoNotConsumeFollowingPositionals(t *testing.T) {
	if got := firstPositional([]string{"--no-login", "https://example.test/alice/work"}); got != "https://example.test/alice/work" {
		t.Fatalf("first positional after boolean flag = %q", got)
	}
	if got := firstPositional([]string{"--remember", "push", "-p", "secret"}); got != "push" {
		t.Fatalf("first positional around mixed flags = %q", got)
	}
}

func TestSubcommandSpecificFlagRestrictions(t *testing.T) {
	tests := [][]string{
		{"cxt", "remote", "add", "origin", "https://example.test/a/b", "-v"},
		{"cxt", "secrets", "pull", "--rotate"},
		{"cxt", "remote", "-v", "add", "origin", "https://example.test/a/b"},
		{"cxt", "push", "--at", "abc"},
		{"cxt", "log", "--yes"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[1:], "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(args); handled || err == nil {
				t.Fatalf("restricted flag = handled %v, err %v", handled, err)
			}
		})
	}
}

func TestHelpCommandTargetsSubcommands(t *testing.T) {
	for _, command := range []string{"setup", "segments", "push", "doctor", "fork"} {
		if handled, err := PreflightArgs([]string{"cxt", "help", command}); err != nil || !handled {
			t.Fatalf("cxt help %s = handled %v, err %v", command, handled, err)
		}
	}
	if handled, err := PreflightArgs([]string{"cxt", "help", "load"}); err == nil || handled {
		t.Fatalf("cxt help for a command that does not exist = handled %v, err %v", handled, err)
	}
}

func TestUnknownCommandUsesPublicCommandCatalog(t *testing.T) {
	err := Run(nil, []string{"cxt", "definitely-unknown"})
	if err == nil {
		t.Fatal("expected an unknown-command error")
	}

	for _, command := range publicCommandNames {
		if !strings.Contains(err.Error(), command) {
			t.Errorf("unknown-command error does not mention %q: %v", command, err)
		}
	}
	if strings.Contains(err.Error(), "git-hook") {
		t.Errorf("unknown-command error exposed internal git-hook command: %v", err)
	}
}

func TestCLIReferenceCoversEveryPublicCommand(t *testing.T) {
	raw, err := os.ReadFile(testutil.RepositoryPath(t, "docs", "CLI.md"))
	if err != nil {
		t.Fatalf("read CLI reference: %v", err)
	}
	doc := string(raw)

	for _, command := range publicCommandNames {
		if !strings.Contains(doc, "cxt "+command) {
			t.Errorf("CLI reference does not mention public command %q", command)
		}
	}
	if strings.Contains(doc, "cxt git-hook") {
		t.Error("CLI reference exposed internal git-hook command")
	}
}
