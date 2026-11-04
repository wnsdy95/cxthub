package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type DocumentPublicationProfile string

const (
	DocumentPublicationCompatibility DocumentPublicationProfile = "compatibility"
	DocumentPublicationComplete      DocumentPublicationProfile = "complete"
)

type DocumentPublicationStore interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	GetSettingsObject(context.Context, domain.ContentHash, domain.ContentHash) (domain.SettingsBundle, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	PutSnapshot(context.Context, domain.Snapshot) error
}

type DocumentPublicationChunkStore interface {
	GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
	PutChunks(context.Context, domain.ContentHash, map[domain.ContentHash][]byte) (int, int, error)
}

type DocumentPublicationLegacyWriter interface {
	PutDoc(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error)
}

type DocumentPublicationSources interface {
	VerifySnapshotDocument(context.Context, domain.ContentHash, domain.Snapshot) error
}

type DocumentPublicationScope interface {
	WithinDocumentCommit(context.Context, domain.ContentHash, func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error)
	WithinChunkStore(context.Context, domain.ContentHash, func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error)
	GraphChanged(context.Context, domain.ContentHash) error
}

type DocumentPublicationCommandsConfig struct {
	Profile             DocumentPublicationProfile
	Store               DocumentPublicationStore
	Chunks              DocumentPublicationChunkStore
	Scope               DocumentPublicationScope
	Sources             DocumentPublicationSources
	Verified            outbound.VerifiedDocStore
	LegacyWriter        DocumentPublicationLegacyWriter
	SupportedIdentities []domain.DocumentIdentity
}

type DocumentPublicationCommands struct {
	profile   DocumentPublicationProfile
	store     DocumentPublicationStore
	chunks    DocumentPublicationChunkStore
	scope     DocumentPublicationScope
	sources   DocumentPublicationSources
	verified  outbound.VerifiedDocStore
	legacy    DocumentPublicationLegacyWriter
	supported []domain.DocumentIdentity
}

var _ inbound.DocumentPublicationCommands = (*DocumentPublicationCommands)(nil)

func NewDocumentPublicationCommands(config DocumentPublicationCommandsConfig) (*DocumentPublicationCommands, error) {
	if config.Profile != DocumentPublicationCompatibility && config.Profile != DocumentPublicationComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Chunks) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, optional := range []any{config.Sources, config.Verified, config.LegacyWriter} {
		if optional != nil && !rootAdapterPresent(optional) {
			return nil, domain.ErrValidation
		}
	}
	seen := make(map[domain.DocumentIdentity]bool, len(config.SupportedIdentities))
	for _, identity := range config.SupportedIdentities {
		if identity.Validate() != nil || seen[identity] {
			return nil, domain.ErrValidation
		}
		seen[identity] = true
	}
	if !seen[domain.DocumentIdentityLegacy] {
		return nil, domain.ErrValidation
	}
	if config.Profile == DocumentPublicationComplete && (config.Sources == nil || config.Verified == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	return &DocumentPublicationCommands{
		profile: config.Profile,
		store:   config.Store, chunks: config.Chunks, scope: config.Scope, sources: config.Sources,
		verified: config.Verified, legacy: config.LegacyWriter,
		supported: append([]domain.DocumentIdentity(nil), config.SupportedIdentities...),
	}, nil
}

func (commands *DocumentPublicationCommands) Commit(ctx context.Context, input inbound.CommitInput) (inbound.CommitOutput, error) {
	output, err := commands.scope.WithinDocumentCommit(ctx, input.RepoID, func(bound context.Context) (inbound.CommitOutput, error) {
		return commands.commit(bound, input)
	})
	if err != nil && commands.profile == DocumentPublicationComplete {
		return inbound.CommitOutput{}, err
	}
	return output, err
}

func (commands *DocumentPublicationCommands) StoreChunks(ctx context.Context, input inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	output, err := commands.scope.WithinChunkStore(ctx, input.RepoID, func(bound context.Context) (inbound.StoreChunksOutput, error) {
		return commands.storeChunks(bound, input)
	})
	if err != nil && commands.profile == DocumentPublicationComplete {
		return inbound.StoreChunksOutput{}, err
	}
	return output, err
}
