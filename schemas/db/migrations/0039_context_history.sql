-- Context operations are independent of conversation hashes. Their old/new
-- snapshot roots survive branch movement and are scoped to a cloud repository.
CREATE TABLE IF NOT EXISTS context_history (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id text NOT NULL,
    event jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, id),
    CHECK (id ~ '^[0-9a-f]{32}$')
);
CREATE INDEX IF NOT EXISTS context_history_repo_time ON context_history (repo_id, received_at, id);
