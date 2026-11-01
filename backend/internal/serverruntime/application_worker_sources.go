package serverruntime

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func WorkerRepositories(variable, value string) ([]domain.ContentHash, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	repositories := []domain.ContentHash{}
	seen := map[domain.ContentHash]bool{}
	for _, part := range strings.Split(value, ",") {
		repository := domain.ContentHash(strings.TrimSpace(part))
		if domain.ValidateContentHash(repository) != nil {
			return nil, fmt.Errorf("%s must contain repository content hashes", variable)
		}
		if !seen[repository] {
			repositories = append(repositories, repository)
			seen[repository] = true
		}
	}
	return repositories, nil
}

func ValidateGitHubOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || parsed.Host == "" {
		return fmt.Errorf("GitHub return origin must be an absolute origin")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")) {
		return fmt.Errorf("GitHub return origin must use HTTPS except on loopback")
	}
	return nil
}
