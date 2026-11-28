-- Archiving a session. An archived session is left out of every list, search
-- and read along a branch, and is opened only from the list of archived
-- sessions; nothing of it is removed, and restoring it brings it back. The
-- mark is kept on the session's own row, so that a report a CLI uploads again
-- does not undo it.
ALTER TABLE anchored_sessions
    ADD COLUMN archived_at TIMESTAMPTZ,
    ADD COLUMN archived_by TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT anchored_sessions_archive_mark CHECK ((archived_at IS NULL) = (archived_by = ''));
