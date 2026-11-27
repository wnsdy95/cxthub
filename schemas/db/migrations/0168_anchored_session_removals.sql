-- Removing a session from the server. The session's report goes, with its
-- commit links, summaries and open ranges, and this row stays in its place:
-- while it stands, no report is taken for the session, so that a CLI that
-- still holds the tool's records does not bring back what was removed.
-- Deleting the row lets the session be uploaded again.
--
-- uploader is the account the session belonged to; removed_by is the account
-- that removed it, which is the uploader or someone who manages the
-- repository.
CREATE TABLE anchored_session_removals (
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL CHECK (provider IN ('claude', 'codex')),
    session_id TEXT NOT NULL CHECK (session_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    uploader   TEXT NOT NULL CHECK (btrim(uploader) <> ''),
    removed_by TEXT NOT NULL CHECK (btrim(removed_by) <> ''),
    removed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (repo_id, provider, session_id)
);

CREATE INDEX anchored_session_removals_uploader ON anchored_session_removals (repo_id, uploader, provider, session_id);
