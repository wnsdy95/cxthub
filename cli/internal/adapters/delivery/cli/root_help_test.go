package cli

import (
	"errors"
	"os"
	"path/filepath"
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
