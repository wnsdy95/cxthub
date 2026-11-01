package workerserver

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/githubapp"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/resend"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/webhookdelivery"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func OpenWorker(ctx context.Context) (*serverruntime.WorkerRuntime, error) {
	if strings.TrimSpace(os.Getenv("CXT_EXECUTION_PROFILE")) != outbound.BoundedWorkersProfile {
		return serverruntime.OpenWorker(ctx)
	}
	if err := serverruntime.ValidateWorkerExecutionProfile(); err != nil {
		return nil, err
	}
	environment, err := applicationWorkerEnvironmentFromEnvironment()
	if err != nil {
		return nil, err
	}
	return serverruntime.OpenWorker(ctx, environment.sources)
}

type applicationWorkerEnvironment struct {
	configuration serverruntime.ApplicationWorkerConfig
	github        *githubapp.Client
}

func applicationWorkerEnvironmentFromEnvironment() (applicationWorkerEnvironment, error) {
	var environment applicationWorkerEnvironment
	environment.configuration.NotificationSender = webhookdelivery.New()
	for _, selection := range []struct {
		variable string
		selected *[]domain.ContentHash
	}{
		{"CXT_PENDING_PROJECTION_REPOS", &environment.configuration.PendingRepositories},
		{"CXT_GRAPH_PROJECTION_REPOS", &environment.configuration.GraphRepositories},
		{"CXT_GRAPH_SHADOW_REPOS", &environment.configuration.QualificationRepositories},
		{"CXT_MEMORY_PREPARATION_REPOS", &environment.configuration.MemoryRepositories},
	} {
		repositories, err := serverruntime.WorkerRepositories(selection.variable, os.Getenv(selection.variable))
		if err != nil {
			return applicationWorkerEnvironment{}, err
		}
		*selection.selected = repositories
	}
	if len(environment.configuration.MemoryRepositories) > domain.MaxMemoryReadWorkerRepos {
		return applicationWorkerEnvironment{}, domain.ErrValidation
	}
	slices.Sort(environment.configuration.QualificationRepositories)
	if key := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); key != "" {
		environment.configuration.InvitationSender = resend.New(key)
	}
	keys := []string{"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET"}
	present := 0
	for _, key := range keys {
		if os.Getenv(key) != "" {
			present++
		}
	}
	if present == 0 {
		return environment, nil
	}
	if present != len(keys) {
		return applicationWorkerEnvironment{}, fmt.Errorf("configure all GitHub App environment variables before enabling the worker connection")
	}
	origin := strings.TrimRight(strings.TrimSpace(os.Getenv("CXT_PUBLIC_URL")), "/")
	if err := serverruntime.ValidateGitHubOrigin(origin); err != nil {
		return applicationWorkerEnvironment{}, err
	}
	client, err := githubapp.New(githubapp.Config{
		AppID: os.Getenv(keys[0]), ClientID: os.Getenv(keys[1]), ClientSecret: os.Getenv(keys[2]),
		PrivateKey: os.Getenv(keys[3]), Slug: os.Getenv(keys[4]), CallbackURL: origin + "/api/v1/github/callback",
	})
	if err != nil {
		return applicationWorkerEnvironment{}, fmt.Errorf("invalid worker GitHub App configuration")
	}
	environment.github = client
	return environment, nil
}

func (environment applicationWorkerEnvironment) sources(storage store.Store) (serverruntime.ApplicationWorkerConfig, error) {
	configuration := environment.configuration
	configuration.GitReader = gitevidence.NewGitHub(func() string { return os.Getenv("CXT_GITHUB_TOKEN") })
	if environment.github == nil {
		return configuration, nil
	}
	teams, teamsSupported := storage.(outbound.TeamStore)
	transactions, transactionsSupported := storage.(outbound.IdentityTransactions)
	if !teamsSupported || !transactionsSupported {
		return serverruntime.ApplicationWorkerConfig{}, domain.ErrCatalogUnsupported
	}
	worker, err := app.NewGitHubWorker(app.GitHubWorkerConfig{
		Profile: app.GitHubWorkerComplete, Store: storage, Remote: environment.github,
		Metadata: storage, Blobs: storage, Repositories: storage, Organizations: storage,
		Teams: teams, IdentityTransactions: transactions,
	})
	if err != nil {
		return serverruntime.ApplicationWorkerConfig{}, err
	}
	configuration.GitHubWorker = worker
	configuration.GitReader = gitevidence.NewAuthenticated(func(ctx context.Context, path string) (string, string, string, error) {
		installation, resolved, err := worker.EvidenceInstallation(ctx, path)
		if err != nil {
			return "", "", "", err
		}
		if installation == 0 {
			return os.Getenv("CXT_GITHUB_TOKEN"), "operator", resolved, nil
		}
		token, err := environment.github.InstallationToken(ctx, installation)
		return token, fmt.Sprintf("installation:%d", installation), resolved, err
	})
	return configuration, nil
}
