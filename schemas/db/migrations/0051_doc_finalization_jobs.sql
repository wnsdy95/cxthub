-- Durable acceptance is separate from document publication. Large CIR validation
-- runs outside HTTP requests and repository transactions; version fences retries.
CREATE TABLE doc_finalization_jobs (
 repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id text NOT NULL,
 -- Preserve exact envelope bytes: JSONB rewrites key order/numeric spellings.
 payload bytea NOT NULL,
 state text NOT NULL CHECK (state IN ('waiting','running','retrying','completed','rejected')),
 version bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL,
 next_attempt timestamptz NOT NULL,
 lease_until timestamptz NOT NULL,
 PRIMARY KEY(repo_id,id)
);
CREATE INDEX doc_finalization_due ON doc_finalization_jobs(next_attempt,created_at,id)
 WHERE state IN ('waiting','running','retrying');
CREATE UNIQUE INDEX doc_finalization_one_running_per_repo ON doc_finalization_jobs(repo_id) WHERE state='running';
