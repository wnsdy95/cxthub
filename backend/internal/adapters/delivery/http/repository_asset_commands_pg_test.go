//go:build postgres

package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryAssetPGPoisonFacade struct {
	*app.Service
}

func (*repositoryAssetPGPoisonFacade) UpdateAbout(context.Context, domain.ContentHash, string, string, []string) error {
	panic("actual PostgreSQL about write reached the legacy facade")
}

func (*repositoryAssetPGPoisonFacade) UpdateRepoConfig(context.Context, domain.ContentHash, *string, *bool) error {
	panic("actual PostgreSQL config write reached the legacy facade")
}

func (*repositoryAssetPGPoisonFacade) PatchRepoProfile(context.Context, domain.ContentHash, inbound.RepoProfilePatch) (domain.Repo, error) {
	panic("actual PostgreSQL profile write reached the legacy facade")
}

func (*repositoryAssetPGPoisonFacade) PutSettings(context.Context, domain.ContentHash, domain.SettingsBundle) error {
	panic("actual PostgreSQL settings write reached the legacy facade")
}

func (*repositoryAssetPGPoisonFacade) PutSettingsObject(context.Context, domain.ContentHash, domain.ContentHash, domain.SettingsBundle) error {
	panic("actual PostgreSQL settings object write reached the legacy facade")
}

func (*repositoryAssetPGPoisonFacade) SaveSecrets(context.Context, inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	panic("actual PostgreSQL secrets write reached the legacy facade")
}
