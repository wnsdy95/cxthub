-- The summary a CLI wrote of a session when the session made a commit: what
-- was done, the facts worth keeping and what is left to do. A summary belongs
-- to one session and one commit that a segment of the session ends at. It is
-- kept apart from the report so that a report stays small, and it is read
-- under the same rule as the segment it belongs to.
CREATE TABLE anchored_session_summaries (
    repo_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    session_id TEXT NOT NULL,
    commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    body       JSONB NOT NULL CHECK (jsonb_typeof(body) = 'object'),
    body_hash  TEXT NOT NULL CHECK (body_hash ~ '^sha256:[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (repo_id, provider, session_id, commit_sha),
    FOREIGN KEY (repo_id, provider, session_id) REFERENCES anchored_sessions(repo_id, provider, session_id) ON DELETE CASCADE
);

CREATE INDEX anchored_session_summaries_commit ON anchored_session_summaries (repo_id, commit_sha);
