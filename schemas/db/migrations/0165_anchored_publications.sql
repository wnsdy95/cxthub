-- Which parts of a session are open to the team, and when the server saw each
-- branch of the remote pushed.
--
-- A session is readable by its uploader from the start. A part of it opens to
-- the team when what it explains is pushed. What was opened stays open, so it
-- is stored as ranges of the session's own records, never as segment numbers:
-- cutting the session again, or a forced push that removes the commit, takes
-- nothing back. The application only ever adds to a session's list.
CREATE TABLE anchored_session_publications (
    repo_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    session_id TEXT NOT NULL,
    body       JSONB NOT NULL CHECK (jsonb_typeof(body) = 'array' AND jsonb_array_length(body) >= 1),
    PRIMARY KEY (repo_id, provider, session_id),
    FOREIGN KEY (repo_id, provider, session_id) REFERENCES anchored_sessions(repo_id, provider, session_id) ON DELETE CASCADE
);

-- A push is a branch the server read with another newest commit than at its
-- read before: one row for each time that was seen. Like the rest of what is
-- read from the provider it belongs to one origin and is removed with it.
CREATE TABLE remote_git_pushes (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch  TEXT NOT NULL CHECK (branch <> '' AND octet_length(branch) <= 1024),
    sha     TEXT NOT NULL CHECK (sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (repo_id, branch, seen_at)
);
