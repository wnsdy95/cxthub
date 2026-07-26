-- 0008: Secret ciphertext envelope (end-to-end encryption — server cannot decrypt, transparent storage)
CREATE TABLE IF NOT EXISTS repo_secrets (
    repo_id TEXT PRIMARY KEY REFERENCES repos (id),
    data    JSONB NOT NULL
);
COMMENT ON TABLE repo_secrets IS 'PBKDF2+AES-256-GCM ciphertext (E2E). Plain text/key is never on the server. cxt secrets pull to receive and decrypt.';
