package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// MoveRepoAddress gives the repository stored under fromURL the address its
// workspace is reached at now, toURL. The new address must name the same
// workspace the stored repository belongs to. Without apply nothing is
// changed, and the answer says what would be. An operator runs it with the
// database's credential; no route reaches it.
func (s *Service) MoveRepoAddress(ctx context.Context, fromURL, toURL string, apply bool) (domain.RepoAddressMove, error) {
	mover, ok := s.meta.(outbound.RepoAddressMover)
	if !ok {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: moving a repository's address needs the PostgreSQL store", domain.ErrValidation)
	}
	from, to := domain.SanitizeRemoteURL(fromURL), domain.SanitizeRemoteURL(toURL)
	if from == "" || to == "" {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: both addresses must be absolute URLs", domain.ErrValidation)
	}
	fromID, toID := domain.HashContent([]byte(normalizeGitURL(from))), domain.HashContent([]byte(normalizeGitURL(to)))
	if fromID == toID {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: %s and %s are the same address", domain.ErrValidation, from, to)
	}
	stored, err := s.meta.GetRepo(ctx, fromID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: no repository is stored under %s", domain.ErrNotFound, from)
	}
	if err != nil {
		return domain.RepoAddressMove{}, err
	}
	if stored.RepositoryID == "" {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: the repository stored under %s belongs to no workspace", domain.ErrConflict, from)
	}
	workspace, err := s.repositoryForURL(ctx, to)
	if err != nil {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: %s names no workspace", domain.ErrConflict, to)
	}
	if workspace != stored.RepositoryID {
		return domain.RepoAddressMove{}, fmt.Errorf("%w: %s names another workspace than the one the repository stored under %s belongs to", domain.ErrConflict, to, from)
	}
	moved, err := mover.MoveRepoAddress(ctx, domain.RepoAddressMoveRequest{From: fromID, To: toID, ToURL: to, RepositoryID: stored.RepositoryID, Apply: apply})
	moved.FromURL = stored.RemoteURL
	return moved, err
}
