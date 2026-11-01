package repositoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type ContextReadScope interface {
	ReadContext(context.Context, domain.ContentHash, func(context.Context) error) error
}

type ContextIndexReader interface {
	ReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error)
}

type ContextConfig struct {
	Profile          Profile
	Scope            ContextReadScope
	Metadata         MetadataReader
	History          HistoryReader
	Revisions        RevisionReader
	Archives         ArchiveReader
	Indexes          ContextIndexReader
	CacheScope       ProjectionCacheScope
	Generations      GraphGenerationReader
	Cache            *SegmentCache
	Contributions    ContributionEvidence
	EvidenceMetadata ProjectionMetadataReader
	Trees            evidencequery.GitTreeReader
	Deltas           evidencequery.GitDeltaReader
	Captured         bool
	RequireComplete  bool
	AtomicRead       bool
}

type ContextQueries struct {
	scope            ContextReadScope
	metadata         MetadataReader
	history          HistoryReader
	revisions        RevisionReader
	archives         ArchiveReader
	indexes          ContextIndexReader
	cacheScope       ProjectionCacheScope
	generations      GraphGenerationReader
	cache            *SegmentCache
	contributions    ContributionEvidence
	evidenceMetadata ProjectionMetadataReader
	trees            evidencequery.GitTreeReader
	deltas           evidencequery.GitDeltaReader
	captured         bool
	requireComplete  bool
	atomicRead       bool
}

var _ inbound.ContextQuery = (*ContextQueries)(nil)

func NewContext(config ContextConfig) (*ContextQueries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != PinnedSnapshots {
		return nil, fmt.Errorf("%w: context query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Metadata, config.Indexes, config.CacheScope, config.Cache, config.Contributions, config.EvidenceMetadata} {
		if absent(required) {
			return nil, fmt.Errorf("%w: context query dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	for _, optional := range []any{config.History, config.Revisions, config.Archives, config.Generations, config.Trees, config.Deltas} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: declared context query dependency is nil", domain.ErrCatalogUnsupported)
		}
	}
	if config.Profile == PinnedSnapshots && (config.History == nil || config.Revisions == nil || !config.AtomicRead) {
		return nil, domain.ErrCatalogUnsupported
	}
	return &ContextQueries{scope: config.Scope, metadata: config.Metadata, history: config.History,
		revisions: config.Revisions, archives: config.Archives, indexes: config.Indexes, cacheScope: config.CacheScope,
		generations: config.Generations, cache: config.Cache, contributions: config.Contributions,
		evidenceMetadata: config.EvidenceMetadata, trees: config.Trees, deltas: config.Deltas,
		captured: config.Captured, requireComplete: config.RequireComplete, atomicRead: config.AtomicRead}, nil
}
