//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.IdentityKeyStore = (*PostgresStore)(nil)

func (s *PostgresStore) ListIdentitySecrets(ctx context.Context, enterprise, after string, limit int) ([]domain.IdentitySecret, error) {
	if enterprise != "" && domain.ValidateEnterpriseID(enterprise) != nil {
		return nil, domain.ErrValidation
	}
	k, id, err := domain.IdentitySecretCursor(after)
	if err != nil || limit < 1 || limit > 101 {
		return nil, domain.ErrValidation
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT kind,id,enterprise_id,revision,sealed FROM (
 SELECT 'oidc_connection' AS kind,enterprise_id AS id,enterprise_id,record->>'revision' AS revision,sealed_secret AS sealed FROM enterprise_oidc_connections
 UNION ALL
 SELECT 'oidc_verifier',hash,enterprise_id,record->>'ConnectionRevision',record->>'Verifier' FROM enterprise_oidc_attempts
 UNION ALL
 SELECT 'saml_key',enterprise_id,enterprise_id,record->>'revision',sealed_key FROM enterprise_saml_connections
 UNION ALL
 SELECT 'saml_alternate',enterprise_id,enterprise_id,record->'rotation'->>'id',sealed_alternate_key FROM enterprise_saml_connections WHERE record->'rotation' IS NOT NULL OR sealed_alternate_key<>''
) x WHERE (kind,id)>($1,$2) AND ($4='' OR enterprise_id=$4) ORDER BY kind,id LIMIT $3`, k, id, limit, enterprise)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.IdentitySecret{}
	for rows.Next() {
		var r domain.IdentitySecret
		if err = rows.Scan(&r.Kind, &r.ID, &r.EnterpriseID, &r.Revision, &r.Sealed); err != nil {
			return nil, err
		}
		if _, err = r.Purpose(); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ReplaceIdentitySecret(ctx context.Context, old domain.IdentitySecret, sealed string) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if _, err := old.Purpose(); err != nil {
		return err
	}
	if sealed == "" {
		return domain.ErrValidation
	}
	var query string
	switch old.Kind {
	case "oidc_connection":
		query = `UPDATE enterprise_oidc_connections SET sealed_secret=$3 WHERE enterprise_id=$1 AND sealed_secret=$2 AND record->>'revision'=$4`
	case "saml_key":
		query = `UPDATE enterprise_saml_connections SET sealed_key=$3 WHERE enterprise_id=$1 AND sealed_key=$2 AND record->>'revision'=$4`
	case "saml_alternate":
		query = `UPDATE enterprise_saml_connections SET sealed_alternate_key=$3 WHERE enterprise_id=$1 AND sealed_alternate_key=$2 AND record->'rotation'->>'id'=$4`
	case "oidc_verifier":
		query = `UPDATE enterprise_oidc_attempts SET record=jsonb_set(record,'{Verifier}',to_jsonb($3::text)) WHERE hash=$1 AND record->>'Verifier'=$2 AND record->>'ConnectionRevision'=$4`
	default:
		return domain.ErrValidation
	}
	r, err := s.db(ctx).Exec(ctx, query, old.ID, old.Sealed, sealed, old.Revision)
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) GetIdentityKeyBatch(ctx context.Context, operation, after string) (b domain.IdentityKeyBatch, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT record FROM identity_key_batches WHERE operation=$1 AND after_cursor=$2`, operation, after).Scan(&b)
	if err != nil {
		return b, mapNoRows(err)
	}
	if b.Operation != operation || b.After != after || !b.Applied || b.ActiveKey == "" || b.Actor == "" || b.Reason == "" || b.CreatedAt.IsZero() || b.Limit < 1 || b.Limit > 100 || b.Scanned < 0 || b.Scanned > b.Limit || b.Changed < 0 || b.Changed > b.Candidates || b.Candidates > b.Scanned {
		return domain.IdentityKeyBatch{}, domain.ErrIntegrity
	}
	if _, _, err := domain.IdentitySecretCursor(b.Next); err != nil {
		return domain.IdentityKeyBatch{}, domain.ErrIntegrity
	}
	return b, nil
}
func (s *PostgresStore) PutIdentityKeyBatch(ctx context.Context, b domain.IdentityKeyBatch) error {
	if err := s.requireIdentityTx(ctx); err != nil {
		return err
	}
	if !b.Applied || b.Operation == "" || b.Actor == "" || b.Reason == "" || b.ActiveKey == "" {
		return domain.ErrValidation
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO identity_key_batches(operation,after_cursor,record,created_at) VALUES($1,$2,$3,$4)`, b.Operation, b.After, raw, b.CreatedAt)
	return mapPGConstraint(err)
}
