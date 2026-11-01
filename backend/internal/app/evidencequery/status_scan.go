package evidencequery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type Scope interface {
	ReadRepository(context.Context, domain.ContentHash, func(context.Context) error) error
}

type ScanReader interface {
	ListGitScans(context.Context, domain.ContentHash, string, int) ([]domain.GitScanJob, error)
}

type HeadScanReader interface {
	GetGitHeadScan(context.Context, domain.ContentHash, string) (domain.GitHeadScan, error)
}

type ScanConfig struct {
	Scope      Scope
	Metadata   MetadataReader
	Scans      ScanReader
	Heads      HeadScanReader
	AtomicRead bool
}

type ScanQueries struct {
	scope      Scope
	metadata   MetadataReader
	scans      ScanReader
	heads      HeadScanReader
	atomicRead bool
}

func NewScanQueries(config ScanConfig) (*ScanQueries, error) {
	for _, required := range []any{config.Scope, config.Metadata, config.Scans} {
		if absent(required) {
			return nil, domain.ErrValidation
		}
	}
	if config.Heads != nil && absent(config.Heads) {
		return nil, domain.ErrCatalogUnsupported
	}
	return &ScanQueries{scope: config.Scope, metadata: config.Metadata, scans: config.Scans,
		heads: config.Heads, atomicRead: config.AtomicRead}, nil
}

func (queries *ScanQueries) ListScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitScanPage, error) {
	var out domain.GitScanPage
	err := queries.scope.ReadRepository(ctx, repo, func(bound context.Context) error {
		out = domain.GitScanPage{Items: []domain.GitScanJob{}}
		if domain.ValidateContentHash(repo) != nil || limit < 1 || limit > 100 {
			return domain.ErrValidation
		}
		if cursor != "" && domain.ValidateGitChangeID(cursor) != nil {
			return domain.ErrValidation
		}
		items, err := queries.scans.ListGitScans(bound, repo, cursor, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			out.NextCursor = items[limit-1].ID
			items = items[:limit]
		}
		out.Items = items
		if queries.heads != nil {
			metadata, err := queries.metadata.GetRepo(bound, repo)
			if err != nil {
				return err
			}
			head, err := queries.heads.GetGitHeadScan(bound, repo, metadata.GitRemoteURL)
			if err == nil {
				out.Reconciliation = &head
			} else if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil && queries.atomicRead {
		return domain.GitScanPage{}, err
	}
	return out, err
}
