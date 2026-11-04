package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type MemoryDigestWriter interface {
	PutMemoryDigest(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error)
	PutMemoryDigestCAS(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error)
}

type MemoryDigestBlobStore interface {
	GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	PutMemory(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error)
}

type MemoryDigestSnapshotStore interface {
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	CompareAndSwapSnapshotMemory(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, domain.ContentHash) error
}

type MemoryDigestWriteScope interface {
	WithinMemoryObjectWrite(context.Context, domain.ContentHash, func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error)
	WithinMemoryAttachmentWrite(context.Context, domain.ContentHash, func(context.Context) error) error
}

type MemoryDigestCommandsConfig struct {
	Blobs     MemoryDigestBlobStore
	Snapshots MemoryDigestSnapshotStore
	Scope     MemoryDigestWriteScope
}

type MemoryDigestCommands struct {
	blobs     MemoryDigestBlobStore
	snapshots MemoryDigestSnapshotStore
	scope     MemoryDigestWriteScope
}

var _ MemoryDigestWriter = (*MemoryDigestCommands)(nil)

func NewMemoryDigestCommands(config MemoryDigestCommandsConfig) (*MemoryDigestCommands, error) {
	if !rootAdapterPresent(config.Blobs) || !rootAdapterPresent(config.Snapshots) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	return &MemoryDigestCommands{blobs: config.Blobs, snapshots: config.Snapshots, scope: config.Scope}, nil
}

func (commands *MemoryDigestCommands) PutMemoryDigest(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	ctx = auditOperation(ctx, "context.memory.updated")
	if digest.ClaimsVersion != 0 || digest.HasMemoryClaims() {
		return "", fmt.Errorf("%w: typed memory requires the typed attachment endpoint", domain.ErrValidation)
	}
	if digest.PreviousMemoryHash != "" {
		return "", fmt.Errorf("%w: causal memory requires the attachment endpoint", domain.ErrValidation)
	}
	return commands.putMemoryDigest(ctx, repo, digest, "", false)
}

func (commands *MemoryDigestCommands) PutMemoryDigestCAS(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	ctx = auditOperation(ctx, "context.memory.updated")
	return commands.putMemoryDigest(ctx, repo, digest, digest.PreviousMemoryHash, true)
}

func validateMemoryDigest(repo domain.ContentHash, digest domain.MemoryDigest) error {
	if err := digest.ValidateMemoryClaims(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	if err := validateHashes(repo, digest.SnapshotID); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(digest.PreviousMemoryHash); err != nil {
		return err
	}
	for _, fragment := range digest.Fragments {
		if err := domain.ValidateContentHash(fragment.SourceSnapshot); err != nil {
			return err
		}
	}
	if coverage := digest.GraftCoverage; coverage != nil {
		if coverage.ProjectionVersion == 0 || coverage.GraftSeq > domain.MaxGraftSeq {
			return fmt.Errorf("%w: invalid memory graft coverage version or sequence", domain.ErrIntegrity)
		}
		if coverage.ProjectionComplete && coverage.LineageFingerprint == "" {
			return fmt.Errorf("%w: complete memory projection is missing its lineage fingerprint", domain.ErrIntegrity)
		}
		if coverage.LineageFingerprint != "" {
			if err := domain.ValidateContentHash(coverage.LineageFingerprint); err != nil {
				return err
			}
		}
		for _, parent := range coverage.GraftParents {
			if err := domain.ValidateContentHash(parent); err != nil {
				return err
			}
		}
		for _, source := range coverage.PinnedSources {
			if err := domain.ValidateContentHash(source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (commands *MemoryDigestCommands) putMemoryDigest(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest, expected domain.ContentHash, causal bool) (domain.ContentHash, error) {
	if err := validateMemoryDigest(repo, digest); err != nil {
		return "", err
	}
	if _, err := commands.snapshots.GetSnapshot(ctx, repo, digest.SnapshotID); err != nil {
		return "", err
	}
	if causal && expected != "" {
		previous, err := commands.blobs.GetMemory(ctx, repo, expected)
		if err != nil {
			return "", err
		}
		if previous.SnapshotID != digest.SnapshotID {
			return "", fmt.Errorf("%w: memory parent belongs to another snapshot", domain.ErrIntegrity)
		}
		if previous.ClaimsVersion > digest.ClaimsVersion {
			return "", fmt.Errorf("%w: memory claims cannot be downgraded; upgrade the client and pull before retrying", domain.ErrValidation)
		}
	}
	hash, err := commands.scope.WithinMemoryObjectWrite(ctx, repo, func(bound context.Context) (domain.ContentHash, error) {
		return commands.blobs.PutMemory(bound, repo, digest)
	})
	if err != nil {
		return "", err
	}
	if err := commands.scope.WithinMemoryAttachmentWrite(ctx, repo, func(bound context.Context) error {
		return commands.snapshots.CompareAndSwapSnapshotMemory(bound, repo, digest.SnapshotID, expected, hash)
	}); err != nil {
		return "", err
	}
	return hash, nil
}
