package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
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
		if command == "help" || commandArgSpecs[command].passthrough {
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
		if spec, ok := commandArgSpecs[command]; !ok || spec.usage == "" {
			t.Errorf("public command %q has no argument/usage spec", command)
		}
	}
}

func TestUnknownFlagsAreRejectedBeforeCommandDispatch(t *testing.T) {
	for command, spec := range commandArgSpecs {
		if spec.passthrough {
			continue
		}
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
		{"cxt", "commit", "-m"},
		{"cxt", "save", "--provider"},
		{"cxt", "load", "--mode"},
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

func TestValueFlagsSupportInlineValues(t *testing.T) {
	tests := []struct {
		args []string
		name string
		want string
	}{
		{args: []string{"-m=-starts-with-hyphen"}, name: "-m", want: "-starts-with-hyphen"},
		{args: []string{"--provider=codex"}, name: "--provider", want: "codex"},
		{args: []string{"-p=-secret"}, name: "-p", want: "-secret"},
	}
	for _, tt := range tests {
		if got := flagVal(tt.args, tt.name); got != tt.want {
			t.Errorf("flagVal(%v, %q) = %q, want %q", tt.args, tt.name, got, tt.want)
		}
		if got := positionals(tt.args); len(got) != 0 {
			t.Errorf("positionals(%v) = %v", tt.args, got)
		}
	}

	if handled, err := PreflightArgs([]string{"cxt", "commit", "-m=-starts-with-hyphen"}); handled || err != nil {
		t.Fatalf("inline value preflight = handled %v, err %v", handled, err)
	}
	if handled, err := PreflightArgs([]string{"cxt", "push", "--force=true"}); handled || err == nil || !strings.Contains(err.Error(), "does not take a value") {
		t.Fatalf("boolean inline value = handled %v, err %v", handled, err)
	}
	if handled, err := PreflightArgs([]string{"cxt", "commit", "-m="}); handled || err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("empty inline value = handled %v, err %v", handled, err)
	}
}

func TestProviderHelpIsPassedThroughWithoutContextPreparation(t *testing.T) {
	for _, command := range []string{"claude", "codex"} {
		for _, flag := range []string{"-h", "--help", "--version"} {
			t.Run(command+"/"+flag, func(t *testing.T) {
				root, _ := providerLaunchFixture(t, command, "")
				args := []string{"cxt", command, flag}
				if handled, err := PreflightArgs(args); err != nil || handled {
					t.Fatalf("provider help intercepted by CXT: handled=%v err=%v", handled, err)
				}
				if _, err := os.Stat(filepath.Join(root, "launch.log")); !os.IsNotExist(err) {
					t.Fatal("preflight started provider")
				}
				c := &Container{ProviderLaunch: ProviderLaunchHooks{
					Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
						t.Fatal("provider diagnostics prepared context")
						return PreparedProviderLaunch{}, nil
					},
					Record: func(context.Context, ProviderLaunchReceipt) error {
						t.Fatal("provider diagnostics recorded package delivery")
						return nil
					},
				}}
				if err := Run(c, args); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(root, "launch.log"))
				if err != nil || string(got) != flag+"\n" {
					t.Fatalf("provider argv=%q err=%v", got, err)
				}
			})
		}
	}
}

func TestProviderUnknownOptionsFailBeforeCompositionAndLiteralArgsStayLiteral(t *testing.T) {
	for _, command := range []string{"claude", "codex"} {
		for _, prefix := range [][]string{{"cxt"}, {"cxt", "--pull"}} {
			args := append(append([]string(nil), prefix...), command, "--provider-owned-flag")
			if handled, err := PreflightArgs(args); handled || err == nil || !strings.Contains(err.Error(), "cannot safely classify") {
				t.Fatalf("unclassified provider launch %v: handled=%v err=%v", args, handled, err)
			}
			if err := Run(nil, args); err == nil || !strings.Contains(err.Error(), "cannot safely classify") {
				t.Fatalf("unclassified launch reached composition: %v", err)
			}
		}
		argv := []string{"--", "--help", "--provider-owned-flag"}
		intent, recognized, err := ParseLaunchIntent(append([]string{command}, argv...))
		if err != nil || !recognized || !reflect.DeepEqual(intent.ProviderArgs, argv) {
			t.Fatalf("literal provider argv changed: %+v recognized=%v err=%v", intent, recognized, err)
		}
		details, err := (ProviderLaunchRequest{Intent: intent}).ArgumentDetails()
		if err != nil || details.Mode != string(providerFresh) || !reflect.DeepEqual(details.Prompts, argv[1:]) {
			t.Fatalf("literal prompt became diagnostic/options: %+v err=%v", details, err)
		}
		// CXT's wrapper help remains available without starting the provider.
		if err := Run(nil, []string{"cxt", "help", command}); err != nil {
			t.Fatalf("wrapper help: %v", err)
		}
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
		{"cxt", "stash", "pop", "-m", "message"},
		{"cxt", "stash", "list", "--provider", "codex"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[1:], "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(args); handled || err == nil {
				t.Fatalf("restricted flag = handled %v, err %v", handled, err)
			}
		})
	}
}

func TestLocalMCPRequiresExplicitFlagBeforeComposition(t *testing.T) {
	if handled, err := PreflightArgs([]string{"cxt", "mcp"}); handled || err == nil ||
		!strings.Contains(err.Error(), "https://cxthub.com/mcp") || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("bare cxt mcp = handled %v, err %v", handled, err)
	}
	if handled, err := PreflightArgs([]string{"cxt", "mcp", "--local"}); handled || err != nil {
		t.Fatalf("cxt mcp --local = handled %v, err %v", handled, err)
	}
}

func TestHelpCommandTargetsSubcommands(t *testing.T) {
	if handled, err := PreflightArgs([]string{"cxt", "help", "load"}); err != nil || !handled {
		t.Fatalf("cxt help load = handled %v, err %v", handled, err)
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
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	docPath := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../../docs/CLI.md"))
	raw, err := os.ReadFile(docPath)
	if errors.Is(err, os.ErrNotExist) {
		// A separately packaged cli module may not include repository-level
		// documentation. The public repository preflight requires the file.
		t.Skip("repository-level CLI reference is not present in this module package")
	}
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
