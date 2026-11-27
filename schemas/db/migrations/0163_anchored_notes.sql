-- Manual notes members write about commits, branches and worktrees, and the
-- worktree lists members' CLIs report for their machines. A note is kept apart
-- from captured sessions and stays when a session is linked to its target
-- later. A worktree list holds names, branches and machine labels, never a
-- path, and is read only by the member who reported it.
CREATE TABLE anchored_notes (
    repo_id     TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id          TEXT NOT NULL CHECK (id ~ '^note_[0-9a-f]{32}$'),
    author      TEXT NOT NULL CHECK (btrim(author) <> ''),
    target_kind TEXT NOT NULL CHECK (target_kind IN ('commit', 'branch', 'worktree')),
    created_at  TIMESTAMPTZ NOT NULL,
    body        JSONB NOT NULL,
    PRIMARY KEY (repo_id, id)
);

CREATE INDEX anchored_notes_author ON anchored_notes (repo_id, author, created_at, id);

CREATE TABLE reported_worktrees (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    account TEXT NOT NULL CHECK (btrim(account) <> ''),
    body    JSONB NOT NULL,
    PRIMARY KEY (repo_id, account)
);
