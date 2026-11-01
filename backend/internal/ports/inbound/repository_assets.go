package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryAssetCommands interface {
	PatchRepoProfile(context.Context, domain.ContentHash, RepoProfilePatch) (domain.Repo, error)
	UpdateAbout(context.Context, domain.ContentHash, string, string, []string) error
	UpdateRepoConfig(context.Context, domain.ContentHash, *string, *bool) error
	PutSettings(context.Context, domain.ContentHash, domain.SettingsBundle) error
	PutSettingsObject(context.Context, domain.ContentHash, domain.ContentHash, domain.SettingsBundle) error
	SaveSecrets
}
