-- External identity binding is explicit; email never participates in a key.
CREATE TABLE IF NOT EXISTS enterprise_oidc_connections (
 enterprise_id TEXT PRIMARY KEY REFERENCES enterprises(id),
 record JSONB NOT NULL,
 sealed_secret TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS enterprise_oidc_attempts (
 hash TEXT PRIMARY KEY,
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
 record JSONB NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 consumed BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS enterprise_oidc_attempt_expiry ON enterprise_oidc_attempts(expires_at);
CREATE TABLE IF NOT EXISTS enterprise_federation_identities (
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
 protocol TEXT NOT NULL,
 issuer TEXT NOT NULL,
 subject TEXT NOT NULL,
 user_id TEXT NOT NULL REFERENCES users(id),
 record JSONB NOT NULL,
 PRIMARY KEY(enterprise_id,protocol,issuer,subject),
 UNIQUE(enterprise_id,protocol,user_id)
);
CREATE TABLE IF NOT EXISTS enterprise_federation_sessions (
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
 session_hash TEXT NOT NULL,
 record JSONB NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(enterprise_id,session_hash)
);
CREATE INDEX IF NOT EXISTS enterprise_federation_session_expiry ON enterprise_federation_sessions(expires_at);
