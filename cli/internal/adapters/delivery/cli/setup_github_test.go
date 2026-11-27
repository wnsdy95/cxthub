package cli

import (
	"errors"
	"testing"

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
