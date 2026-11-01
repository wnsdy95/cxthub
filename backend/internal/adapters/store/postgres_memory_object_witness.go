//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.WitnessedMemoryReader = (*PostgresStore)(nil)

func (storage *PostgresStore) CaptureMemoryObject(ctx context.Context, repo, hash domain.ContentHash, maxBytes int) (domain.CapturedMemoryObject, error) {
	witness := domain.MemoryObjectWitness{RepoID: repo, Hash: hash}
	digest, err := storage.getMemoryForCapture(ctx, repo, hash, maxBytes, &witness)
	if canceled := ctx.Err(); canceled != nil {
		return domain.CapturedMemoryObject{}, canceled
	}
	if errors.Is(err, domain.ErrNotFound) {
		return domain.CapturedMemoryObject{Witness: domain.MemoryObjectWitness{RepoID: repo, Hash: hash}}, nil
	}
	if err != nil {
		return domain.CapturedMemoryObject{}, err
	}
	witness.Available = true
	slices.SortFunc(witness.Components, func(left, right domain.MemoryObjectVersion) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	if err := witness.Validate(); err != nil {
		return domain.CapturedMemoryObject{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.CapturedMemoryObject{}, err
	}
	return domain.CapturedMemoryObject{Digest: &digest, Witness: witness}, nil
}

func (storage *PostgresStore) ValidateMemoryObjectWitnesses(ctx context.Context, repo domain.ContentHash, witnesses []domain.MemoryObjectWitness) error {
	transaction, err := storage.memoryCaptureTransaction(ctx)
	if err != nil {
		return err
	}
	if domain.ValidateContentHash(repo) != nil {
		return domain.ErrValidation
	}
	if transaction.repo != "" && transaction.repo != repo {
		return domain.ErrConflict
	}
	return validateMemoryObjectWitnesses(ctx, transaction, repo, witnesses)
}

func validateMemoryObjectWitnesses(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, witnesses []domain.MemoryObjectWitness) error {
	if len(witnesses) > domain.MaxMemoryCaptureObjects {
		return domain.ErrMemoryProjectionLimit
	}
	set := domain.NewMemoryObjectWitnessSet(repo)
	for _, witness := range witnesses {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := set.Add(witness); err != nil {
			return err
		}
	}
	ordered, err := set.Objects()
	if err != nil {
		return err
	}
	for offset := 0; offset < len(ordered); offset += 128 {
		batch := ordered[offset:min(offset+128, len(ordered))]
		kinds, hashes := make([]string, len(batch)), make([]string, len(batch))
		for ordinal, identity := range batch {
			kinds[ordinal], hashes[ordinal] = identity.Kind, string(identity.Hash)
		}
		rows, err := transaction.Query(ctx, `SELECT owned.hash IS NOT NULL,source.incarnation::text,body.doc_range_source_version
 FROM unnest($2::text[],$3::text[]) WITH ORDINALITY AS wanted(kind,hash,position)
 LEFT JOIN repo_blobs owned ON owned.repo_id=$1 AND owned.kind=wanted.kind AND owned.hash=wanted.hash
 LEFT JOIN blobs body ON body.hash=owned.hash
 LEFT JOIN blob_source_incarnations source ON source.hash=body.hash
 ORDER BY wanted.position`, string(repo), kinds, hashes)
		if err != nil {
			return err
		}
		ordinal := 0
		for rows.Next() {
			var owned bool
			var incarnation *string
			var version *int64
			if err := rows.Scan(&owned, &incarnation, &version); err != nil {
				rows.Close()
				return err
			}
			if ordinal >= len(batch) {
				rows.Close()
				return domain.ErrIntegrity
			}
			wanted := batch[ordinal].Version
			ordinal++
			if owned && (incarnation == nil || version == nil) {
				rows.Close()
				return domain.ErrIntegrity
			}
			if wanted == nil && owned || wanted != nil && (!owned || wanted.Incarnation != *incarnation || wanted.Version != *version) {
				rows.Close()
				return domain.ErrConflict
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if ordinal != len(batch) {
			return domain.ErrIntegrity
		}
	}
	return ctx.Err()
}
