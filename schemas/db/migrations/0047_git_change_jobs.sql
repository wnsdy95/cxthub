-- Git change verification and its terminal evidence are one durable atomic row.
-- Independent immutable pairs may be verified in parallel; lease versions fence
-- expired workers. This queue never mutates PR completion receipts or refs.
CREATE TABLE IF NOT EXISTS git_change_jobs (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id TEXT NOT NULL,
 payload JSONB NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('waiting','running','retrying','completed','attention')),
 next_attempt TIMESTAMPTZ NOT NULL,
 lease_until TIMESTAMPTZ NOT NULL,
 version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
 PRIMARY KEY(repo_id,id)
);
CREATE INDEX IF NOT EXISTS git_change_jobs_due ON git_change_jobs(next_attempt,id)
 WHERE state IN ('waiting','running','retrying');
