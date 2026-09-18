-- Provider-verified commit roots and content-addressed directory objects.
-- The observation worker publishes these with its lease fence and revision.
CREATE TABLE IF NOT EXISTS git_tree_nodes (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 origin TEXT NOT NULL, oid TEXT NOT NULL, payload JSONB NOT NULL,
 PRIMARY KEY(repo_id,origin,oid)
);
CREATE TABLE IF NOT EXISTS git_commit_trees (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 origin TEXT NOT NULL, commit_oid TEXT NOT NULL, payload JSONB NOT NULL,
 PRIMARY KEY(repo_id,origin,commit_oid)
);
-- Completed older scans remain discoverable for one replayable tree upgrade.
CREATE INDEX IF NOT EXISTS git_scan_tree_upgrade ON git_scan_jobs(next_attempt,id)
 WHERE state='completed' AND NOT coalesce((payload->>'tree_indexed')::boolean,false);

-- Evidence discovery must not invalidate the complete context graph.
ALTER TABLE repository_revisions ADD COLUMN IF NOT EXISTS evidence BIGINT NOT NULL DEFAULT 0;
