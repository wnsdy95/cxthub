package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type recordingCommandSessionResolver struct {
	selection outbound.CommandSessionSelection
	failure   error
	cwd       string
	explicit  domain.ProviderKind
	calls     int
}

func (resolver *recordingCommandSessionResolver) ResolveCommandSession(_ context.Context, cwd string, explicit domain.ProviderKind) (outbound.CommandSessionSelection, error) {
	resolver.cwd, resolver.explicit = cwd, explicit
	resolver.calls++
	return resolver.selection, resolver.failure
}

func (*recordingCommandSessionResolver) SupervisedProvider(context.Context, string) (domain.ProviderKind, bool) {
	return "", false
}

func TestSaveAndStashUseInjectedCommandSessionSelection(test *testing.T) {
	for _, command := range []string{"save", "stash"} {
		test.Run(command, func(test *testing.T) {
			test.Chdir(test.TempDir())
			cwd, err := os.Getwd()
			if err != nil {
				test.Fatal(err)
			}
			test.Setenv("CODEX_THREAD_ID", "11111111-1111-4111-8111-111111111111")
			selected := outbound.CommandSessionSelection{Provider: domain.ProviderCodex, SessionPath: filepath.Join(cwd, "selected-source"), SessionID: "selected-id"}
			resolver := &recordingCommandSessionResolver{selection: selected}
			save, stash := &recordingSave{}, &recordingStash{}
			container := &Container{SessionSelection: resolver, Save: save, Stash: stash, Memorize: noOpMemorize{}}
			arguments := []string{"cxt", command}
			if command == "stash" {
				arguments = append(arguments, "push")
			}
			arguments = append(arguments, "--provider", "codex")
			if err := Run(container, arguments); err != nil {
				test.Fatal(err)
			}
			if resolver.calls != 1 || resolver.cwd != cwd || resolver.explicit != domain.ProviderCodex {
				test.Fatal("command did not delegate its cwd and provider selection exactly once", resolver)
			}
			provider, path := save.in.Provider, save.in.SessionPath
			if command == "stash" {
				provider, path = stash.in.Provider, stash.in.SessionPath
			}
			if provider != selected.Provider || path != selected.SessionPath {
				test.Fatal("command replaced the selected native input", provider, path)
			}
		})
	}
}

func TestCommandSessionSelectionFailureDoesNotSave(test *testing.T) {
	for _, configured := range []bool{false, true} {
		test.Run(map[bool]string{false: "missing-composition", true: "missing-native-source"}[configured], func(test *testing.T) {
			test.Chdir(test.TempDir())
			save := &recordingSave{}
			container := &Container{Save: save, Memorize: noOpMemorize{}}
			if configured {
				container.SessionSelection = &recordingCommandSessionResolver{failure: domain.ErrNoActiveSession}
			}
			err := Run(container, []string{"cxt", "save", "--provider", "codex"})
			if err == nil || save.in.Provider != "" || save.in.SessionPath != "" {
				test.Fatal("selection failure fell through to implicit capture", save.in, err)
			}
			if configured && !errors.Is(err, domain.ErrNoActiveSession) || !configured && !strings.Contains(err.Error(), "command session selection unavailable") {
				test.Fatal("selection failure changed", err)
			}
		})
	}
}
