package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryAssetCommands interface {
	PatchRepoProfile(context.Context, domain.ContentHash, RepoProfilePatch) (domain.Repo, error)
	PutSettings(context.Context, domain.ContentHash, domain.SettingsBundle) error
	SaveSecrets
}
