package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var _ inbound.SaveSecrets = (*Service)(nil)

func (service *Service) SaveSecrets(ctx context.Context, input inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return inbound.SaveSecretsOutput{}, err
	}
	return commands.SaveSecrets(ctx, input)
}
