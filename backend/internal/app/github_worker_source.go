package app

import (
	"context"
	"errors"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (worker *GitHubWorker) EvidenceInstallation(ctx context.Context, path string) (int64, string, error) {
	repoID := outbound.GitRepository(ctx)
	if repoID == "" {
		return 0, "", domain.ErrForbidden
	}
	repo, err := worker.metadata.GetRepo(ctx, repoID)
	if err != nil {
		return 0, "", err
	}
	local, err := worker.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return 0, "", err
	}
	connection, err := worker.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return 0, path, nil
	}
	if err != nil {
		return 0, "", err
	}
	for _, binding := range connection.Bindings {
		if binding.ContextRepoID != repoID {
			continue
		}
		current, err := worker.binding(ctx, connection, binding)
		if err != nil {
			return 0, "", err
		}
		name := strings.TrimPrefix(normalizeGitURL(current.GitRemoteURL), "github.com/")
		prefix := "/repos/" + name + "/"
		if !strings.HasPrefix(strings.ToLower(path), prefix) {
			return 0, "", domain.ErrForbidden
		}
		for _, repository := range connection.Repositories {
			if repository.ID == binding.ExternalID {
				return connection.Installation.ID, "/repos/" + repository.FullName + "/" + path[len(prefix):], nil
			}
		}
	}
	return 0, "", domain.ErrForbidden
}
