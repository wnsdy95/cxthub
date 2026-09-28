CREATE TABLE IF NOT EXISTS enterprise_saml_connections (
 enterprise_id TEXT PRIMARY KEY REFERENCES enterprises(id),
 record JSONB NOT NULL,
 metadata TEXT NOT NULL,
 sealed_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS enterprise_saml_attempts (
 hash TEXT PRIMARY KEY,
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
 record JSONB NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 finish_hash TEXT UNIQUE,
 received BOOLEAN NOT NULL DEFAULT false,
 completed BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS enterprise_saml_attempt_expiry ON enterprise_saml_attempts(expires_at);
-- A provider must not reuse an assertion ID, even across different requests.
CREATE TABLE IF NOT EXISTS enterprise_saml_replays (
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id),
 issuer TEXT NOT NULL,
 assertion_id TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(enterprise_id,issuer,assertion_id)
);
