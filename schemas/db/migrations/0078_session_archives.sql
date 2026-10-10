CREATE TABLE session_archives (
    repo_id     TEXT NOT NULL REFERENCES repos(id),
    key         TEXT NOT NULL CHECK (key ~ '^sha256:[0-9a-f]{64}$'),
    snapshot_id TEXT NOT NULL CHECK (snapshot_id ~ '^sha256:[0-9a-f]{64}$'),
    provider    TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL,
    archived_by TEXT NOT NULL CHECK (btrim(archived_by) <> ''),
    PRIMARY KEY (repo_id, key),
    FOREIGN KEY (repo_id, snapshot_id) REFERENCES snapshots(repo_id, id)
);
