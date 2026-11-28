package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// What cxt setup says of the repository's connection with GitHub: only a
// repository the server says is not connected is missing something.
func TestSetupGitHubLine(t *testing.T) {
	const page = "https://example.test/connect/github"
	for name, test := range map[string]struct {
		connection domain.GitHubRepositoryConnection
		err        error
		mark, line string
	}{
		"connected": {domain.GitHubRepositoryConnection{State: domain.GitHubRepositoryConnected, ConnectURL: page}, nil,
			"✓", "GitHub connected (the server reads what is pushed through the GitHub App)"},
		"not connected": {domain.GitHubRepositoryConnection{State: domain.GitHubRepositoryNotConnected, ConnectURL: page}, nil,
			"✗", "GitHub is not connected — a maintainer of the repository connects it at " + page},
		"a server without a GitHub App": {domain.GitHubRepositoryConnection{State: "not_configured"}, nil, "", ""},
		"a server that does not say":    {domain.GitHubRepositoryConnection{}, nil, "", ""},
		"a server that failed to say": {domain.GitHubRepositoryConnection{State: domain.GitHubRepositoryNotConnected, ConnectURL: page}, errors.New("synthetic status 500"),
			"⚠", "the server did not say whether this repository is connected to GitHub: synthetic status 500"},
	} {
		if mark, line := setupGitHubLine(test.connection, test.err); mark != test.mark || line != test.line {
			t.Errorf("%s: %q %q", name, mark, line)
		}
	}
	if setupMissing != "✗" {
		t.Fatalf("the mark of what is missing is %q", setupMissing)
	}
}

// What cxt setup says of automatic upload, in a repository as it is after a
// fresh setup and in one where it was turned off: the line says which, and
// how to change it.
func TestSetupAutomaticUploadLine(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if mark, line := setupAutomaticUploadLine(remotecfg.SegmentsAuto(root)); mark != "✓" || line != "automatic upload on: your Claude Code and Codex sessions in this repository go to the server on their own as you work; sessions that ended before it began go with cxt push (to stop it: cxt config segments.auto off)" {
		t.Fatalf("a fresh setup: %q %q", mark, line)
	}
	if err := remotecfg.SetSegmentsAuto(ctx, root, "off"); err != nil {
		t.Fatal(err)
	}
	if mark, line := setupAutomaticUploadLine(remotecfg.SegmentsAuto(root)); mark != "⚠" || line != "automatic upload off in this repository: sessions go to the server only with cxt push (to turn it on: cxt config segments.auto on)" {
		t.Fatalf("turned off: %q %q", mark, line)
	}
}
