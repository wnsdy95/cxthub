//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Verify the complete document identity with bounded component memory. This
// reproduces the public chunk-format contract; it never trusts a read/search cache.
func verifyFrozenDoc(ctx context.Context, source *FSStore, repo, want domain.ContentHash, stored []byte) error {
	if source == nil {
		return domain.ErrNotFound
	}
	_, err := verifyFrozenDocWithReader(ctx, want, stored, source.ownedFSChunkReader(repo))
	return err
}
