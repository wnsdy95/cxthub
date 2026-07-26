package cli

import (
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
