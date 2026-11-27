//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) RequireDocumentIdentity(ctx context.Context, repo domain.ContentHash, next domain.DocumentIdentity) error {
	if err := next.Validate(); err != nil {
		return err
	}
	return s.WithinRepository(ctx, repo, func(tx context.Context) error {
		r, err := s.GetRepo(tx, repo)
		if err != nil {
			return err
		}
		if err = domain.ValidateDocumentIdentityRequirement(r.RequiredDocIdentity, next); err != nil {
			return err
		}
		if r.RequiredDocIdentity == next {
			return nil
		}
		_, err = s.db(tx).Exec(tx, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(repo), string(next))
		return storageWriteError(mapPGConstraint(err))
	})
}
