package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// CatalogImageInstaller publishes a fully reconciled metadata image by CAS.
// It must verify existing cache bytes first and preserve the old image on error.
// This operation cannot create body receipts, observations or working state.
type CatalogImageInstaller interface {
	InstallCatalogImage(context.Context, domain.ContentHash, string, string, domain.CatalogCheckpoint, []domain.CatalogEntry) (domain.ContentHash, error)
}
