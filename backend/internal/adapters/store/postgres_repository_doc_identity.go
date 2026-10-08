//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

// The caller supplies its existing snapshot or bounded cache transaction.
// FOR SHARE pins the requirement until cache publication commits; no graph lock
// is held while source pages are collected or hashed.
func (s *PostgresStore) checkRepositoryDocumentIdentity(ctx context.Context, db catalogMerkleReader, repo domain.ContentHash, pin bool) error {
	query := `SELECT required_doc_identity FROM repos WHERE id=$1`
	if pin {
		query += ` FOR SHARE`
	}
	var required domain.DocumentIdentity
	if err := db.QueryRow(ctx, query, string(repo)).Scan(&required); err != nil {
		return mapNoRows(err)
	}
	return outbound.CheckDocumentIdentityCompatibility(ctx, required)
}
