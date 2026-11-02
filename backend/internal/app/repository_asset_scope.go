package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryAssetScope struct {
	work            *repositoryWork
	rootPublication bool
	identityStore   outbound.RepositoryDocumentIdentityStore
	transactional   bool
}

var _ RepositoryAssetWriteScope = (*repositoryAssetScope)(nil)

func (service *Service) RepositoryAssetCommands() (inbound.RepositoryAssetCommands, error) {
	return service.repositoryAssets(RepositoryAssetsComplete)
}

func (service *Service) repositoryAssets(profile RepositoryAssetProfile) (*RepositoryAssetCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	work := service.repositoryWork()
	if profile == RepositoryAssetsComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
	}
	secrets, _ := service.meta.(outbound.SecretsCASStore)
	notifications, _ := service.meta.(RepositoryNotificationQueue)
	if profile == RepositoryAssetsCompatibility {
		legacy, _ := service.meta.(outbound.NotificationStore)
		notifications = legacy
	}
	identities, _ := service.meta.(outbound.RepositoryDocumentIdentityStore)
	transactional := work.transactions != nil
	return NewRepositoryAssetCommands(RepositoryAssetCommandsConfig{
		Profile: profile, Store: service.meta, Scope: &repositoryAssetScope{work: work,
			rootPublication: service.RootPublicationEnabled(), identityStore: identities, transactional: transactional},
		Repositories: service.repositories, SecretsCAS: secrets, Notifications: notifications,
	})
}

func (scope *repositoryAssetScope) WithinAssetWrite(ctx context.Context, repo domain.ContentHash, operation RepositoryAssetOperation, write func(context.Context) error) error {
	if scope == nil || scope.work == nil || write == nil {
		return domain.ErrValidation
	}
	switch operation {
	case AssetUpdateAbout:
		ctx = writeAction(auditOperation(ctx, "repository.about.updated"), "manage")
	case AssetUpdateConfig:
		ctx = writeAction(auditOperation(ctx, "repository.config.updated"), "manage")
	case AssetPutSettings:
		ctx = writeAction(auditOperation(ctx, "repository.settings.updated"), "settings")
	case AssetPutSettingsObject:
		ctx = context.WithValue(auditOperation(ctx, "repository.settings.object.stored"), revisionScopeKey{}, "none")
	case AssetSaveSecrets:
		ctx = auditOperation(ctx, "repository.secrets.updated")
	case AssetPatchProfile:
		ctx = writeAction(auditOperation(ctx, "repository.profile.updated"), "manage")
	default:
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(ctx, scope.work, repo, write)
}

func (scope *repositoryAssetScope) AuthorizeAssetSecrets(ctx context.Context, input inbound.SaveSecretsInput) error {
	if scope == nil || scope.work == nil {
		return domain.ErrForbidden
	}
	if scope.work.configurationError != nil {
		return scope.work.configurationError
	}
	return scope.work.authority.AuthorizeSecrets(ctx, input.RepoID, input.ActorID)
}

func (scope *repositoryAssetScope) WithinAssetSecretsWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.SaveSecretsOutput{}, domain.ErrValidation
	}
	return repositoryWriteWithWork(auditOperation(ctx, "repository.secrets.updated"), scope.work, repo, write)
}

func (scope *repositoryAssetScope) WithinAssetProfileWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.Repo, error)) (domain.Repo, error) {
	if scope == nil || scope.work == nil || write == nil {
		return domain.Repo{}, domain.ErrValidation
	}
	ctx = writeAction(auditOperation(ctx, "repository.profile.updated"), "manage")
	return repositoryWriteWithWork(ctx, scope.work, repo, write)
}

func (scope *repositoryAssetScope) RequireAssetDocumentIdentity(ctx context.Context, repo domain.ContentHash, next domain.DocumentIdentity) error {
	if scope == nil || scope.work == nil {
		return domain.ErrValidation
	}
	current, err := scope.work.meta.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err := domain.ValidateDocumentIdentityRequirement(current.RequiredDocIdentity, next); err != nil {
		return err
	}
	if next == domain.DocumentIdentityLegacy {
		return nil
	}
	if !scope.rootPublication {
		return domain.ErrRootPublicationDisabled
	}
	if !hasDocumentIdentity(inbound.DocumentIdentities(ctx), next) || !hasDocumentIdentity(scope.work.supported, next) {
		return domain.ErrDocumentIdentityUpgradeRequired
	}
	if !scope.transactional {
		return fmt.Errorf("%w: repository transaction unavailable", domain.ErrRootPublicationDisabled)
	}
	if scope.identityStore == nil {
		return domain.ErrRootPublicationDisabled
	}
	return scope.identityStore.RequireDocumentIdentity(ctx, repo, next)
}
