-- Immutable provider objects and restartable inverse-change discovery.
CREATE TABLE IF NOT EXISTS git_scan_jobs (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id TEXT NOT NULL, payload JSONB NOT NULL, state TEXT NOT NULL,
 next_attempt TIMESTAMPTZ NOT NULL, lease_until TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(repo_id,id)
);
CREATE INDEX IF NOT EXISTS git_scan_jobs_due ON git_scan_jobs(next_attempt,id)
 WHERE state IN ('waiting','retrying','running');
CREATE TABLE IF NOT EXISTS git_commit_deltas (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id TEXT NOT NULL, origin TEXT NOT NULL, commit_oid TEXT NOT NULL,
 parent_oid TEXT NOT NULL, payload JSONB NOT NULL,
 keys TEXT[] NOT NULL, inverse_keys TEXT[] NOT NULL,
 PRIMARY KEY(repo_id,id), UNIQUE(repo_id,origin,commit_oid,parent_oid)
);
CREATE INDEX IF NOT EXISTS git_commit_deltas_keys ON git_commit_deltas USING GIN(keys);
CREATE TABLE IF NOT EXISTS git_ref_observations (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id TEXT NOT NULL, payload JSONB NOT NULL, observed_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(repo_id,id)
);
CREATE TABLE IF NOT EXISTS git_head_scans (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 origin TEXT NOT NULL, payload JSONB NOT NULL,
 PRIMARY KEY(repo_id,origin)
);
