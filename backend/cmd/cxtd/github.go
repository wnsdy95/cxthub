package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/githubapp"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

func configureGitHub(id *app.IdentityService, core *app.Service, st store.Store, publicURL string) (*app.GitHubConnections, *githubapp.Client, error) {
	keys := []string{"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET"}
	present := 0
	for _, key := range keys {
		if os.Getenv(key) != "" {
			present++
		}
	}
	if present == 0 {
		return nil, nil, nil
	}
	if present != len(keys) {
		return nil, nil, fmt.Errorf("configure all GitHub App environment variables before enabling the connection")
	}
	if err := validateGitHubOrigin(publicURL); err != nil {
		return nil, nil, err
	}
	if _, err := githubReturnURL(publicURL); err != nil {
		return nil, nil, err
	}
	client, err := githubapp.New(githubapp.Config{AppID: os.Getenv(keys[0]), ClientID: os.Getenv(keys[1]), ClientSecret: os.Getenv(keys[2]), PrivateKey: os.Getenv(keys[3]), Slug: os.Getenv(keys[4]), CallbackURL: publicURL + "/api/v1/github/callback"})
	if err != nil {
		return nil, nil, err
	}
	return app.NewGitHubConnections(id, core, st, client), client, nil
}

func githubReturnURL(publicURL string) (string, error) {
	origin := strings.TrimRight(strings.TrimSpace(os.Getenv("CXT_WEB_URL")), "/")
	if origin == "" {
		origin = publicURL
	}
	if err := validateGitHubOrigin(origin); err != nil {
		return "", err
	}
	return origin + "/connect/github", nil
}

func validateGitHubOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Host == "" {
		return fmt.Errorf("GitHub return origin must be an absolute origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return fmt.Errorf("GitHub return origin must use HTTPS except on loopback")
	}
	return nil
}
