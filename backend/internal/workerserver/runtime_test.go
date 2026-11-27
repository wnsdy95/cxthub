package workerserver

import (
	"testing"

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
