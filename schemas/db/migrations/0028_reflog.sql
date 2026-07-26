-- 0028: ref movement append-only record (git reflog response). Each time ref moves, it preserves the previous target (old).
-- It keeps, as a safety net, the recovery evidence for tips that cannot be reached via ref movement/graft/gc. It is read-only.
-- It only serves as a safety net and does not change the lineage — it does not force reconnection but provides evidence of "what was there".
CREATE TABLE IF NOT EXISTS reflog (
    id         bigserial PRIMARY KEY,
    repo_id    text NOT NULL,
    kind       text NOT NULL,
    name       text NOT NULL,
    old        text NOT NULL DEFAULT '',
    new        text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS reflog_repo_idx ON reflog (repo_id, id DESC);
