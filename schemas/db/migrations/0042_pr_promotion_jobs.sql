-- Durable delivery state is separate from immutable PR source/completion receipts.
CREATE TABLE pr_promotion_jobs (
 repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
 id TEXT NOT NULL,
 payload JSONB NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('waiting','retrying','running','completed','attention')),
 created_at TIMESTAMPTZ NOT NULL,
 next_attempt TIMESTAMPTZ NOT NULL,
 lease_until TIMESTAMPTZ NOT NULL,
 version BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY(repo_id,id)
);
CREATE INDEX pr_promotion_due ON pr_promotion_jobs(next_attempt,created_at)
 WHERE state IN ('waiting','retrying','running');
