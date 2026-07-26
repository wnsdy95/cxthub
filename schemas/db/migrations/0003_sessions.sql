-- cxt PostgreSQL DDL — Migration 0003: Login Session
-- Apply order: psql -f 0003_sessions.sql (after 0002)
--
-- Flow: Client sends IDP token (Firebase ID token / dev token) to POST /auth/session →
-- Server issues and stores session token (sess_<hex>) → Subsequent requests use session token as Bearer.
-- Server controls logout (session deletion) and expiration to avoid Firebase revalidation per request.

CREATE TABLE IF NOT EXISTS sessions (
    token       TEXT        PRIMARY KEY,                 -- sess_<hex>
    user_id     TEXT        NOT NULL REFERENCES users (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_user_idx    ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);

COMMENT ON TABLE sessions IS 'Server login session. Issued by IDP token exchange. Expiration and logout controlled by server (stateless IDP validation).';
COMMENT ON COLUMN sessions.token IS 'sess_<hex>. Sent by client as Authorization: Bearer. Expired sessions are rejected and deleted.';
