package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RepositoryAssetProfile string

const (
	RepositoryAssetsCompatibility RepositoryAssetProfile = "compatibility"
	RepositoryAssetsComplete      RepositoryAssetProfile = "complete"
)

type RepositoryAssetOperation uint8

const (
	AssetUpdateAbout RepositoryAssetOperation = iota + 1
	AssetUpdateConfig
	AssetPutSettings
	AssetPutSettingsObject
	AssetSaveSecrets
	AssetPatchProfile
)

type RepositoryAssetStore interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	UpdateRepoAbout(context.Context, domain.ContentHash, string, string, []string) error
	UpdateRepoConfig(context.Context, domain.ContentHash, *string, *bool) error
	PutSettingsBundle(context.Context, domain.ContentHash, domain.SettingsBundle) error
	PutSettingsObject(context.Context, domain.ContentHash, domain.ContentHash, domain.SettingsBundle) error
	GetSecretsEnvelope(context.Context, domain.ContentHash) ([]byte, error)
}

type RepositoryAssetRepositoryReader interface {
	GetRepository(context.Context, string) (domain.Repository, error)
}

type RepositoryAssetWriteScope interface {
	WithinAssetWrite(context.Context, domain.ContentHash, RepositoryAssetOperation, func(context.Context) error) error
	WithinAssetSecretsWrite(context.Context, domain.ContentHash, func(context.Context) (inbound.SaveSecretsOutput, error)) (inbound.SaveSecretsOutput, error)
	WithinAssetProfileWrite(context.Context, domain.ContentHash, func(context.Context) (domain.Repo, error)) (domain.Repo, error)
	AuthorizeAssetSecrets(context.Context, inbound.SaveSecretsInput) error
	RequireAssetDocumentIdentity(context.Context, domain.ContentHash, domain.DocumentIdentity) error
}

type RepositoryAssetCommandsConfig struct {
	Profile       RepositoryAssetProfile
	Store         RepositoryAssetStore
	Scope         RepositoryAssetWriteScope
	Repositories  RepositoryAssetRepositoryReader
	SecretsCAS    outbound.SecretsCASStore
	Notifications RepositoryNotificationQueue
}

type RepositoryAssetCommands struct {
	store         RepositoryAssetStore
	scope         RepositoryAssetWriteScope
	repositories  RepositoryAssetRepositoryReader
	secrets       outbound.SecretsCASStore
	notifications RepositoryNotificationQueue
}

var _ inbound.RepositoryAssetCommands = (*RepositoryAssetCommands)(nil)

func NewRepositoryAssetCommands(config RepositoryAssetCommandsConfig) (*RepositoryAssetCommands, error) {
	if config.Profile != RepositoryAssetsCompatibility && config.Profile != RepositoryAssetsComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Repositories, config.SecretsCAS, config.Notifications} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
		if config.Profile == RepositoryAssetsComplete && dependency == nil {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	return &RepositoryAssetCommands{store: config.Store, scope: config.Scope, repositories: config.Repositories,
		secrets: config.SecretsCAS, notifications: config.Notifications}, nil
}

func (commands *RepositoryAssetCommands) UpdateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	return commands.scope.WithinAssetWrite(ctx, repo, AssetUpdateAbout, func(bound context.Context) error {
		return commands.updateAbout(bound, repo, description, website, topics)
	})
}

func (commands *RepositoryAssetCommands) PatchRepoProfile(ctx context.Context, repo domain.ContentHash, patch inbound.RepoProfilePatch) (domain.Repo, error) {
	return commands.scope.WithinAssetProfileWrite(ctx, repo, func(bound context.Context) (domain.Repo, error) {
		current, err := commands.store.GetRepo(bound, repo)
		if err != nil {
			return domain.Repo{}, err
		}
		if patch.RequiredDocIdentity != nil {
			if err := commands.scope.RequireAssetDocumentIdentity(bound, repo, *patch.RequiredDocIdentity); err != nil {
				return domain.Repo{}, err
			}
		}
		if patch.DefaultBranch != nil && *patch.DefaultBranch != "" {
			if err := domain.ValidateBranchName(*patch.DefaultBranch); err != nil {
				return domain.Repo{}, err
			}
		}
		if patch.Description != nil {
			current.Description = *patch.Description
		}
		if patch.Website != nil {
			current.Website = *patch.Website
		}
		if patch.Topics != nil {
			current.Topics = patch.Topics
		}
		if _, err := normalizeAboutWebsite(current.Website); err != nil {
			return domain.Repo{}, err
		}
		if patch.DefaultBranch != nil || patch.ProtectDefault != nil {
			if err := commands.updateConfig(bound, repo, patch.DefaultBranch, patch.ProtectDefault); err != nil {
				return domain.Repo{}, err
			}
		}
		if patch.Description != nil || patch.Website != nil || patch.Topics != nil {
			if err := commands.updateAbout(bound, repo, current.Description, current.Website, current.Topics); err != nil {
				return domain.Repo{}, err
			}
		}
		return commands.store.GetRepo(bound, repo)
	})
}

func (commands *RepositoryAssetCommands) updateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	normalized, err := normalizeAboutWebsite(website)
	if err != nil {
		return err
	}
	if len(topics) > 20 {
		topics = topics[:20]
	}
	return commands.store.UpdateRepoAbout(ctx, repo, strings.TrimSpace(description), normalized, topics)
}

func (commands *RepositoryAssetCommands) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	return commands.scope.WithinAssetWrite(ctx, repo, AssetUpdateConfig, func(bound context.Context) error {
		return commands.updateConfig(bound, repo, branch, protected)
	})
}

func (commands *RepositoryAssetCommands) updateConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if branch != nil && *branch != "" {
		if err := domain.ValidateBranchName(*branch); err != nil {
			return err
		}
	}
	return commands.store.UpdateRepoConfig(ctx, repo, branch, protected)
}

func (commands *RepositoryAssetCommands) PutSettings(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	return commands.scope.WithinAssetWrite(ctx, repo, AssetPutSettings, func(bound context.Context) error {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
			return err
		}
		bundle.UpdatedAt = time.Now().UTC()
		return commands.store.PutSettingsBundle(bound, repo, bundle)
	})
}

func (commands *RepositoryAssetCommands) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	return commands.scope.WithinAssetWrite(ctx, repo, AssetPutSettingsObject, func(bound context.Context) error {
		if err := validateHashes(repo, hash); err != nil {
			return err
		}
		if err := domain.ValidateSettingsBundle(bundle.Kind, hash, bundle); err != nil {
			return err
		}
		return commands.store.PutSettingsObject(bound, repo, hash, bundle)
	})
}

func (commands *RepositoryAssetCommands) SaveSecrets(ctx context.Context, input inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	ctx = inbound.WithRepositoryActor(ctx, input.ActorID)
	if err := domain.ValidateContentHash(input.RepoID); err != nil {
		return inbound.SaveSecretsOutput{}, err
	}
	if input.ActorID == "" {
		return inbound.SaveSecretsOutput{}, domain.ErrUnauthorized
	}
	if commands.secrets == nil {
		return inbound.SaveSecretsOutput{}, domain.ErrSecretsConsistency
	}
	return commands.scope.WithinAssetSecretsWrite(ctx, input.RepoID, func(bound context.Context) (inbound.SaveSecretsOutput, error) {
		if err := commands.scope.AuthorizeAssetSecrets(bound, input); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		previous, err := commands.store.GetSecretsEnvelope(bound, input.RepoID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return inbound.SaveSecretsOutput{}, fmt.Errorf("%w: %w", domain.ErrSecretsConsistency, err)
		}
		if errors.Is(err, domain.ErrNotFound) {
			previous = nil
		}
		if err := domain.ValidateSecretsEdit(previous, input.Envelope, input.Edit); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		next, err := domain.WithSecretsRevision(input.Envelope, "")
		if err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		if len(next) > domain.MaxSecretsEnvelopeBytes {
			return inbound.SaveSecretsOutput{}, fmt.Errorf("%w: envelope exceeds 256KiB", domain.ErrIntegrity)
		}
		if err := commands.secrets.CompareAndSwapSecrets(bound, input.RepoID, previous, next); err != nil {
			if errors.Is(err, domain.ErrRefConflict) {
				return inbound.SaveSecretsOutput{}, domain.ErrSecretsConflict
			}
			return inbound.SaveSecretsOutput{}, err
		}
		if err := commands.notifySecretsChanged(bound, input.RepoID); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		return inbound.SaveSecretsOutput{Status: "stored", Revision: domain.SecretsRevision(next)}, nil
	})
}

func (commands *RepositoryAssetCommands) notifySecretsChanged(ctx context.Context, repo domain.ContentHash) error {
	if commands.repositories == nil {
		return nil
	}
	metadata, err := commands.store.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if metadata.RepositoryID == "" {
		return nil
	}
	record, err := commands.repositories.GetRepository(ctx, metadata.RepositoryID)
	if err != nil {
		return err
	}
	name := metadata.RemoteURL
	if position := strings.LastIndex(name, "/"); position >= 0 {
		name = name[position+1:]
	}
	return enqueueRepositoryNotificationWithQueue(ctx, commands.notifications, record, "secrets_updated",
		fmt.Sprintf("cxthub: %s — secrets updated (team members run cxt secrets pull)", name))
}

func normalizeAboutWebsite(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > 2048 {
		return "", fmt.Errorf("%w: website URL is too long", domain.ErrValidation)
	}
	for _, character := range value {
		if character <= 0x1f || character == 0x7f || unicode.IsSpace(character) || character == '\\' {
			return "", fmt.Errorf("%w: website must be a single-line URL", domain.ErrValidation)
		}
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return "", fmt.Errorf("%w: website must be a valid URL", domain.ErrValidation)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: website scheme must be http or https", domain.ErrValidation)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%w: website URL must not contain credentials", domain.ErrValidation)
	}
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.Path == "/" && parsed.RawQuery == "" && parsed.Fragment == "" {
		parsed.Path = ""
	}
	normalized := parsed.String()
	if len(normalized) > 2048 {
		return "", fmt.Errorf("%w: website URL is too long", domain.ErrValidation)
	}
	return normalized, nil
}
