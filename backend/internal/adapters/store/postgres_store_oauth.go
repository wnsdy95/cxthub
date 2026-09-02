//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OAuthStore = (*PostgresStore)(nil)

func (s *PostgresStore) CreateOAuthClient(ctx context.Context, client domain.OAuthClient) error {
	if err := domain.ValidateOAuthClientRecord(client); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, created_at) VALUES ($1,$2,$3,$4)`,
		client.ID, client.Name, client.RedirectURIs, client.CreatedAt)
	return err
}

func (s *PostgresStore) GetOAuthClient(ctx context.Context, clientID string) (domain.OAuthClient, error) {
	if err := domain.ValidateExternalID(clientID); err != nil {
		return domain.OAuthClient{}, err
	}
	var client domain.OAuthClient
	err := s.pool.QueryRow(ctx,
		`SELECT client_id, client_name, redirect_uris, created_at FROM oauth_clients WHERE client_id=$1`, clientID).
		Scan(&client.ID, &client.Name, &client.RedirectURIs, &client.CreatedAt)
	if err != nil {
		return domain.OAuthClient{}, mapNoRows(err)
	}
	if client.ID != clientID || domain.ValidateOAuthClientRecord(client) != nil {
		return domain.OAuthClient{}, domain.ErrIntegrity
	}
	return client, nil
}

func (s *PostgresStore) CreateOAuthAuthorizationRequest(ctx context.Context, req domain.OAuthAuthorizationRequest) error {
	if err := domain.ValidateOAuthAuthorizationRequestRecord(req); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`WITH expired AS (
		   DELETE FROM oauth_authorization_requests WHERE expires_at <= now()
		 )
		 INSERT INTO oauth_authorization_requests
		 (id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		req.ID, req.ClientID, req.RedirectURI, req.State, req.CodeChallenge, req.Resource, req.Scope, req.CreatedAt, req.ExpiresAt)
	return err
}

func scanOAuthRequest(row pgx.Row) (domain.OAuthAuthorizationRequest, error) {
	var req domain.OAuthAuthorizationRequest
	err := row.Scan(&req.ID, &req.ClientID, &req.RedirectURI, &req.State, &req.CodeChallenge, &req.Resource, &req.Scope, &req.CreatedAt, &req.ExpiresAt)
	if err != nil {
		return domain.OAuthAuthorizationRequest{}, mapNoRows(err)
	}
	if err := domain.ValidateOAuthAuthorizationRequestRecord(req); err != nil {
		return domain.OAuthAuthorizationRequest{}, domain.ErrIntegrity
	}
	return req, nil
}

func (s *PostgresStore) GetOAuthAuthorizationRequest(ctx context.Context, requestID string) (domain.OAuthAuthorizationRequest, error) {
	if err := domain.ValidateExternalID(requestID); err != nil {
		return domain.OAuthAuthorizationRequest{}, err
	}
	return scanOAuthRequest(s.pool.QueryRow(ctx,
		`SELECT id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at
		 FROM oauth_authorization_requests WHERE id=$1`, requestID))
}

func (s *PostgresStore) ApproveOAuthAuthorizationRequest(ctx context.Context, requestID, userID, codeHash string, codeExpiresAt time.Time) (domain.OAuthAuthorizationCode, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	req, err := scanOAuthRequest(tx.QueryRow(ctx,
		`DELETE FROM oauth_authorization_requests WHERE id=$1 AND expires_at > now()
		 RETURNING id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at`, requestID))
	if err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	now := time.Now().UTC()
	if !codeExpiresAt.After(now) {
		return domain.OAuthAuthorizationCode{}, domain.ErrValidation
	}
	code := domain.OAuthAuthorizationCode{
		CodeHash: codeHash, ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		UserID: userID, CodeChallenge: req.CodeChallenge, Resource: req.Resource,
		Scope: req.Scope, CreatedAt: now, ExpiresAt: codeExpiresAt,
	}
	if err := domain.ValidateOAuthAuthorizationCodeRecord(code); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM oauth_authorization_codes WHERE expires_at <= now()`); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO oauth_authorization_codes
		 (code_hash, client_id, redirect_uri, user_id, code_challenge, resource, scope, created_at, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		code.CodeHash, code.ClientID, code.RedirectURI, code.UserID, code.CodeChallenge, code.Resource, code.Scope, code.CreatedAt, code.ExpiresAt); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.OAuthAuthorizationCode{}, err
	}
	return code, nil
}

func (s *PostgresStore) DenyOAuthAuthorizationRequest(ctx context.Context, requestID string) (domain.OAuthAuthorizationRequest, error) {
	return scanOAuthRequest(s.pool.QueryRow(ctx,
		`DELETE FROM oauth_authorization_requests WHERE id=$1 AND expires_at > now()
		 RETURNING id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at`, requestID))
}

func scanOAuthCode(row pgx.Row) (domain.OAuthAuthorizationCode, error) {
	var code domain.OAuthAuthorizationCode
	err := row.Scan(&code.CodeHash, &code.ClientID, &code.RedirectURI, &code.UserID, &code.CodeChallenge, &code.Resource, &code.Scope, &code.CreatedAt, &code.ExpiresAt)
	if err != nil {
		return domain.OAuthAuthorizationCode{}, mapNoRows(err)
	}
	if err := domain.ValidateOAuthAuthorizationCodeRecord(code); err != nil {
		return domain.OAuthAuthorizationCode{}, domain.ErrIntegrity
	}
	return code, nil
}

func (s *PostgresStore) ConsumeOAuthAuthorizationCode(ctx context.Context, codeHash, clientID, redirectURI, codeChallenge string) (domain.OAuthAuthorizationCode, error) {
	code, err := scanOAuthCode(s.pool.QueryRow(ctx,
		`DELETE FROM oauth_authorization_codes
		 WHERE code_hash=$1 AND client_id=$2 AND redirect_uri=$3 AND code_challenge=$4 AND expires_at > now()
		 RETURNING code_hash, client_id, redirect_uri, user_id, code_challenge, resource, scope, created_at, expires_at`,
		codeHash, clientID, redirectURI, codeChallenge))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.OAuthAuthorizationCode{}, domain.ErrUnauthorized
	}
	return code, err
}
