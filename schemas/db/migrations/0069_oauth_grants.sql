-- No guessed grouping of pre-existing tokens by user/client: legacy refresh
-- credentials acquire a grant only when presented successfully.
CREATE TABLE oauth_grants (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id),
    client_id text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (expires_at > created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
ALTER TABLE sessions ADD COLUMN grant_id text REFERENCES oauth_grants(id);
CREATE INDEX sessions_grant_idx ON sessions(grant_id) WHERE grant_id IS NOT NULL;
CREATE TABLE oauth_refresh_references (
    token_hash text PRIMARY KEY,
    grant_id text NOT NULL REFERENCES oauth_grants(id),
    expires_at timestamptz NOT NULL
);
CREATE INDEX oauth_refresh_references_expiry_idx ON oauth_refresh_references(expires_at);
ALTER TABLE account_audit ADD COLUMN grant_id text NOT NULL DEFAULT '';
