CREATE TABLE IF NOT EXISTS account_audit (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL REFERENCES users(id),
 client_id TEXT NOT NULL,
 action TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS account_audit_user ON account_audit(user_id,created_at DESC,id DESC);
