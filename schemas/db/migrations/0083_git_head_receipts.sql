CREATE TABLE git_head_receipts (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> ''),
    version BIGINT NOT NULL CHECK (version > 0),
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, origin, version),
    UNIQUE (repo_id, id)
);
