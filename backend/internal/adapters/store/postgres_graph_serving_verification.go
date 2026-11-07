//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// verifyGraphServingParts checks, inside the promotion transaction, that the
// rows promotion is about to copy are present, live and within output limits,
// and returns the digests the copy must match byte-for-byte (promotion hashes
// each row in the copying statement itself, so it is not hashed twice here).
// It resolves each verified (kind,key) exactly like readReadyPartsFrom does — the latest version at or below the applied
// sequence — so superseded versions and tombstones of other keys never count.
// The verified set includes the header, and the header's digest pins the key
// set, so rows the header does not reference are irrelevant here as well.
func (s *PostgresStore) verifyGraphServingParts(ctx context.Context, transaction pgx.Tx, lease domain.GraphQualificationLease, verified []domain.GraphProjectionPartDigest) ([]string, []string, []string, error) {
	projection := s.graphProjection()
	if len(verified) == 0 || len(verified) > projection.outputRows() {
		return nil, nil, nil, domain.ErrIntegrity
	}
	expected := make(map[[2]string]struct{}, len(verified))
	kinds, keys, digests := make([]string, 0, len(verified)), make([]string, 0, len(verified)), make([]string, 0, len(verified))
	header := false
	for _, part := range verified {
		identity := [2]string{part.Kind, part.Key}
		if _, duplicate := expected[identity]; duplicate {
			return nil, nil, nil, domain.ErrIntegrity
		}
		expected[identity] = struct{}{}
		header = header || identity == [2]string{"header", "view"}
		if domain.ValidateContentHash(part.Digest) != nil {
			return nil, nil, nil, domain.ErrIntegrity
		}
		kinds, keys, digests = append(kinds, part.Kind), append(keys, part.Key), append(digests, string(part.Digest))
	}
	if !header {
		return nil, nil, nil, domain.ErrIntegrity
	}
	rows, err := transaction.Query(ctx, `SELECT wanted.kind,wanted.entity_key,latest.deleted,
		COALESCE(octet_length(latest.image::text),0)+octet_length(wanted.kind)+octet_length(wanted.entity_key)+128
		FROM unnest($4::text[],$5::text[]) AS wanted(kind,entity_key)
		JOIN LATERAL (SELECT deleted,image FROM graph_projection_parts
			WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 AND kind=wanted.kind AND entity_key=wanted.entity_key
			ORDER BY source_seq DESC LIMIT 1) latest ON true`,
		string(lease.RepoID), lease.Dataset, lease.Applied.Sequence, kinds, keys)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var size int64
	for rows.Next() {
		var kind, key string
		var deleted bool
		var bytes int64
		if err := rows.Scan(&kind, &key, &deleted, &bytes); err != nil {
			return nil, nil, nil, err
		}
		identity := [2]string{kind, key}
		if _, ok := expected[identity]; !ok || deleted {
			return nil, nil, nil, domain.ErrIntegrity
		}
		delete(expected, identity)
		if size += bytes; size > int64(projection.outputBytes()) {
			return nil, nil, nil, domain.ErrRepositoryViewSourceLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	if len(expected) != 0 {
		return nil, nil, nil, domain.ErrIntegrity
	}
	return kinds, keys, digests, ctx.Err()
}
