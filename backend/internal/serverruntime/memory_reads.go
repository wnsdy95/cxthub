package serverruntime

import (
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func preparedMemoryRepositories(value string) ([]domain.ContentHash, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) > domain.MaxMemoryReadWorkerRepos {
		return nil, fmt.Errorf("CXT_MEMORY_READ_REPOS exceeds the repository limit")
	}
	seen := make(map[domain.ContentHash]bool, len(parts))
	var repos []domain.ContentHash
	for _, part := range parts {
		repo := domain.ContentHash(strings.TrimSpace(part))
		if domain.ValidateContentHash(repo) != nil {
			return nil, fmt.Errorf("CXT_MEMORY_READ_REPOS requires comma-separated repository hashes")
		}
		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	return repos, nil
}
