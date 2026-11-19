-- Reports CLIs upload for coding-agent sessions: where each session is cut at
-- the commits it made, and an index from a commit to the sessions cut at it.
-- A report is keyed by the tool's own session identifier, never by a document
-- hash, so it does not depend on how session content is stored.
CREATE TABLE anchored_sessions (
    repo_id           TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL CHECK (provider IN ('claude', 'codex')),
    session_id        TEXT NOT NULL CHECK (session_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    uploader          TEXT NOT NULL CHECK (btrim(uploader) <> ''),
    parent_session_id TEXT NOT NULL DEFAULT '',
    body              JSONB NOT NULL,
    body_hash         TEXT NOT NULL CHECK (body_hash ~ '^sha256:[0-9a-f]{64}$'),
    revision          BIGINT NOT NULL CHECK (revision >= 1),
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (repo_id, provider, session_id)
);

CREATE INDEX anchored_sessions_uploader ON anchored_sessions (repo_id, uploader, provider, session_id);

CREATE TABLE anchored_session_commits (
    repo_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    session_id TEXT NOT NULL,
    commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    segment_no INTEGER NOT NULL CHECK (segment_no >= 1),
    rule       TEXT NOT NULL CHECK (rule IN ('printed-number', 'same-folder-and-time')),
    PRIMARY KEY (repo_id, provider, session_id, commit_sha),
    FOREIGN KEY (repo_id, provider, session_id) REFERENCES anchored_sessions(repo_id, provider, session_id) ON DELETE CASCADE
);

CREATE INDEX anchored_session_commits_commit ON anchored_session_commits (repo_id, commit_sha);
