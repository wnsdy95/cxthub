package workerserver

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func wave48ClearWorkerSources(test *testing.T) {
	test.Helper()
	for _, variable := range []string{
		"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS", "CXT_MEMORY_PREPARATION_REPOS",
		"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET", "RESEND_API_KEY",
	} {
		test.Setenv(variable, "")
	}
}

func TestWave48WorkerRepositorySelectionMatchesAPIContract(test *testing.T) {
	first := domain.ContentHash("sha256:" + strings.Repeat("a", 64))
	second := domain.ContentHash("sha256:" + strings.Repeat("b", 64))
	selected, err := serverruntime.WorkerRepositories("CXT_GRAPH_PROJECTION_REPOS", " "+string(second)+","+string(first)+","+string(second)+" ")
	if err != nil || len(selected) != 2 || selected[0] != second || selected[1] != first {
		test.Fatal("selection changed deduplication or encounter order", selected, err)
	}
	selected, err = serverruntime.WorkerRepositories("SELECTION", "  ")
	if err != nil || selected != nil {
		test.Fatal("empty selection changed", selected, err)
	}
	for _, value := range []string{"secret-value", string(first) + ",", "," + string(first)} {
		if _, err := serverruntime.WorkerRepositories("SELECTION", value); err == nil || err.Error() != "SELECTION must contain repository content hashes" {
			test.Fatal("invalid selection was accepted or exposed its value", err)
		}
	}
}

func TestWave48WorkerEnvironmentKeepsOptionalSendersDisabled(test *testing.T) {
	wave48ClearWorkerSources(test)
	environment, err := applicationWorkerEnvironmentFromEnvironment()
	if err != nil || environment.github != nil || environment.configuration.InvitationSender != nil {
		test.Fatal("empty configuration enabled an external provider", err)
	}
	configuration, err := environment.sources(nil)
	if err != nil || configuration.GitReader == nil || configuration.GitHubWorker != nil || configuration.InvitationSender != nil {
		test.Fatal("operator reader construction accessed storage or enabled an unconfigured provider", err)
	}
	test.Setenv("RESEND_API_KEY", "test-key-no-network")
	environment, err = applicationWorkerEnvironmentFromEnvironment()
	if err != nil || environment.configuration.InvitationSender == nil || environment.github != nil {
		test.Fatal("configured mail adapter was not explicitly assembled", err)
	}
}

func TestWave48WorkerGitHubOriginUsesSameAPIPolicy(test *testing.T) {
	for _, origin := range []string{"https://cxthub.example", "http://localhost:5173", "http://127.0.0.1:8907", "http://[::1]:8907"} {
		if err := serverruntime.ValidateGitHubOrigin(origin); err != nil {
			test.Fatal("supported API origin was rejected by the worker", origin, err)
		}
	}
	for _, origin := range []string{"", "http://example.test", "https://user:secret@example.test", "https://example.test/path", "https://example.test?query=1", "https://example.test#fragment"} {
		if err := serverruntime.ValidateGitHubOrigin(origin); err == nil {
			test.Fatal("unsupported API origin accepted by worker", origin)
		}
	}
	wave48ClearWorkerSources(test)
	for _, variable := range []string{"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET"} {
		test.Setenv(variable, "invalid-key-no-network")
	}
	test.Setenv("CXT_PUBLIC_URL", "http://localhost:5173")
	if _, err := applicationWorkerEnvironmentFromEnvironment(); err == nil || err.Error() != "invalid worker GitHub App configuration" {
		test.Fatal("worker rejected the loopback origin before validating its provider key", err)
	}
}

func TestWave48WorkerEnvironmentRejectsInvalidSourcesBeforeStorage(test *testing.T) {
	for _, variable := range []string{"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS", "CXT_MEMORY_PREPARATION_REPOS"} {
		test.Run(variable, func(test *testing.T) {
			wave48ClearWorkerSources(test)
			test.Setenv(variable, "invalid-secret-selection")
			if _, err := applicationWorkerEnvironmentFromEnvironment(); err == nil || strings.Contains(err.Error(), "invalid-secret-selection") {
				test.Fatal("malformed selection accepted or exposed", err)
			}
		})
	}
	test.Run("partial GitHub", func(test *testing.T) {
		wave48ClearWorkerSources(test)
		test.Setenv("CXT_GITHUB_APP_CLIENT_SECRET", "never-print-secret")
		if _, err := applicationWorkerEnvironmentFromEnvironment(); err == nil || strings.Contains(err.Error(), "never-print-secret") {
			test.Fatal("partial GitHub configuration accepted or exposed", err)
		}
	})
	test.Run("invalid GitHub key", func(test *testing.T) {
		wave48ClearWorkerSources(test)
		for _, variable := range []string{"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET"} {
			test.Setenv(variable, "never-print-secret")
		}
		test.Setenv("CXT_PUBLIC_URL", "https://example.test")
		if _, err := applicationWorkerEnvironmentFromEnvironment(); err == nil || err.Error() != "invalid worker GitHub App configuration" {
			test.Fatal("invalid GitHub key accepted or exposed", err)
		}
	})
	test.Run("memory limit", func(test *testing.T) {
		wave48ClearWorkerSources(test)
		var selected []string
		for index := 0; index <= domain.MaxMemoryReadWorkerRepos; index++ {
			selected = append(selected, fmt.Sprintf("sha256:%064x", index))
		}
		test.Setenv("CXT_MEMORY_PREPARATION_REPOS", strings.Join(selected, ","))
		if _, err := applicationWorkerEnvironmentFromEnvironment(); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("memory selection exceeded the original preparation limit", err)
		}
	})
}
