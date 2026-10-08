package storage

import (
	"context"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.RootDocumentStore = (*FileStore)(nil)

// PutConversationManifest installs only an explicitly bound root, after all
// current staged bytes pass the full verifier. Public PutDoc stays legacy-only.
func (s *FileStore) PutConversationManifest(ctx context.Context, rep domain.DocumentRepresentation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manifest, err := rep.ConversationManifest()
	if err != nil {
		return err
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return err
	}
	ref := rep.DocumentRef()
	return s.WithObjectsRetained(ctx, func() error {
		_, err := s.withOSLock(ctx, "root-document", hexOf(ref.Hash), syscall.LOCK_EX, true, func() error {
			path := s.objectPath("docs", ref.Hash)
			if fileExists(path) {
				// Idempotency never repairs a corrupt winner or missing dependency.
				_, err := s.readRootDocument(ctx, ref, false)
				return err
			}
			if _, err := s.verifyRootDocument(ctx, ref, manifest, false); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return writeAtomic(path, docCompress(raw))
		})
		return err
	})
}
