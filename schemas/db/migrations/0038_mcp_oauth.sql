-- 0038: OAuth 2.1 authorization server state for the remote read-only MCP
-- resource. Clients are public PKCE clients; no client secrets are stored.

CREATE TABLE IF NOT EXISTS oauth_clients (
    client_id      TEXT PRIMARY KEY,
    client_name    TEXT NOT NULL,
    redirect_uris  TEXT[] NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS oauth_authorization_requests (
    id              TEXT PRIMARY KEY,
    client_id       TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    redirect_uri    TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT '',
    code_challenge  TEXT NOT NULL,
    resource        TEXT NOT NULL,
    scope           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS oauth_authorization_requests_expiry_idx
    ON oauth_authorization_requests (expires_at);

CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    code_hash       TEXT PRIMARY KEY,
    client_id       TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    redirect_uri    TEXT NOT NULL,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_challenge  TEXT NOT NULL,
    resource        TEXT NOT NULL,
    scope           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS oauth_authorization_codes_expiry_idx
    ON oauth_authorization_codes (expires_at);

COMMENT ON TABLE oauth_authorization_codes IS
    'Single-use MCP OAuth authorization codes. code_hash stores sha256(raw), never the bearer code.';
