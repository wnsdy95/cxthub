CREATE TABLE doc_range_jobs_v1 (
  repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  doc_hash text NOT NULL,
  state text NOT NULL CHECK (state IN ('waiting','running','retrying','completed','rejected')),
  reason text NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 128),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
  created_at timestamptz NOT NULL,
  next_attempt timestamptz NOT NULL,
  lease_until timestamptz,
  PRIMARY KEY (repo_id, doc_hash),
  CHECK ((state = 'running') = (lease_until IS NOT NULL))
);

CREATE INDEX doc_range_jobs_v1_due ON doc_range_jobs_v1(next_attempt, created_at, repo_id, doc_hash)
  WHERE state IN ('waiting','running','retrying');

CREATE UNIQUE INDEX doc_range_jobs_v1_one_running_per_repo ON doc_range_jobs_v1(repo_id)
  WHERE state = 'running';
