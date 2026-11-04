package repositoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type GraphReadScope interface {
	ReadGraph(context.Context, domain.ContentHash, func(context.Context) error) error
}

type LegacyGraphConfig struct {
	Profile    Profile
	Scope      GraphReadScope
	Metadata   MetadataReader
	History    HistoryReader
	Revisions  RevisionReader
	Archives   ArchiveReader
	Projection *LegacyProjection
}

type LegacyGraphQueries struct {
	scope      GraphReadScope
	metadata   MetadataReader
	history    HistoryReader
	revisions  RevisionReader
	archives   ArchiveReader
	projection *LegacyProjection
}

var _ inbound.RepositoryGraphQueries = (*LegacyGraphQueries)(nil)

func NewLegacyGraph(config LegacyGraphConfig) (*LegacyGraphQueries, error) {
	if config.Profile != LegacyCompatibility && config.Profile != PinnedSnapshots {
		return nil, fmt.Errorf("%w: legacy graph query profile is required", domain.ErrValidation)
	}
	for _, required := range []any{config.Scope, config.Metadata, config.Projection} {
		if absent(required) {
			return nil, fmt.Errorf("%w: legacy graph query dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	for _, optional := range []any{config.History, config.Revisions, config.Archives} {
		if optional != nil && absent(optional) {
			return nil, fmt.Errorf("%w: declared legacy graph query dependency is nil", domain.ErrCatalogUnsupported)
		}
	}
	if config.Profile == PinnedSnapshots && (config.History == nil || config.Revisions == nil) {
		return nil, domain.ErrCatalogUnsupported
	}
	return &LegacyGraphQueries{scope: config.Scope, metadata: config.Metadata, history: config.History,
		revisions: config.Revisions, archives: config.Archives, projection: config.Projection}, nil
}
