-- One permanent record for each lifetime of a branch name on the remote. The
-- ledger is separate from context branch identity: it never reads or moves a
-- ref.
CREATE TABLE branch_lifetimes (
    repo_id   TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    ledger_id TEXT NOT NULL CHECK (ledger_id ~ '^[0-9a-f]{32}$'),
    name      TEXT NOT NULL CHECK (name <> '' AND length(name) <= 255),
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    body      JSONB NOT NULL,
    PRIMARY KEY (repo_id, ledger_id)
);

-- A name alive on the remote has exactly one lifetime.
CREATE UNIQUE INDEX branch_lifetimes_alive ON branch_lifetimes (repo_id, name) WHERE closed_at IS NULL;

CREATE INDEX branch_lifetimes_name ON branch_lifetimes (repo_id, name, opened_at);

-- Every ID, a lifetime's own or one joined to it, belongs to one lifetime.
CREATE TABLE branch_ledger_ids (
    repo_id   TEXT NOT NULL,
    id        TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{32}$'),
    ledger_id TEXT NOT NULL,
    PRIMARY KEY (repo_id, id),
    FOREIGN KEY (repo_id, ledger_id) REFERENCES branch_lifetimes(repo_id, ledger_id) ON DELETE CASCADE
);

CREATE INDEX branch_ledger_ids_lifetime ON branch_ledger_ids (repo_id, ledger_id);
