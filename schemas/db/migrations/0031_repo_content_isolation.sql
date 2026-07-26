-- 0031: content identity is global, ownership and graph identity are repo-scoped.
-- The same Git repository and identical context hashes may exist in multiple workspaces.

-- Associate a globally deduplicated blob with each repo that actually supplied it.
CREATE TABLE IF NOT EXISTS repo_blobs (
    repo_id TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('doc', 'memory')),
    hash    TEXT NOT NULL REFERENCES blobs (hash),
    PRIMARY KEY (repo_id, kind, hash)
);
CREATE INDEX IF NOT EXISTS repo_blobs_hash_idx ON repo_blobs (hash);

INSERT INTO repo_blobs (repo_id, kind, hash)
SELECT repo_id, 'doc', doc_hash FROM snapshots
ON CONFLICT DO NOTHING;

INSERT INTO repo_blobs (repo_id, kind, hash)
SELECT repo_id, 'memory', memory_hash FROM snapshots WHERE memory_hash IS NOT NULL
ON CONFLICT DO NOTHING;

-- memories previously inherited repo identity from a globally unique snapshot id.
ALTER TABLE memories ADD COLUMN IF NOT EXISTS repo_id TEXT;
UPDATE memories AS m
SET repo_id = s.repo_id
FROM snapshots AS s
WHERE m.repo_id IS NULL AND m.snapshot_id = s.id;

-- Replace global snapshot identity with repo-scoped identity.
ALTER TABLE refs DROP CONSTRAINT IF EXISTS refs_target_fkey;
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_snapshot_id_fkey;
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_pkey;
ALTER TABLE snapshots DROP CONSTRAINT IF EXISTS snapshots_pkey;

ALTER TABLE snapshots
    ADD CONSTRAINT snapshots_pkey PRIMARY KEY (repo_id, id);

-- A symbolic HEAD has no direct target. NULL lets the composite FK remain strict for
-- every branch, tag, and detached HEAD while supporting symbolic HEAD correctly.
ALTER TABLE refs ALTER COLUMN target DROP NOT NULL;
UPDATE refs SET target = NULL WHERE kind = 'head' AND symbolic <> '';
ALTER TABLE refs
    ADD CONSTRAINT refs_target_repo_fkey
    FOREIGN KEY (repo_id, target) REFERENCES snapshots (repo_id, id);
ALTER TABLE refs
    ADD CONSTRAINT refs_target_or_symbolic_check
    CHECK (
        (kind = 'head' AND ((symbolic <> '' AND target IS NULL) OR (symbolic = '' AND target IS NOT NULL)))
        OR (kind <> 'head' AND symbolic = '' AND target IS NOT NULL)
    );

ALTER TABLE memories ALTER COLUMN repo_id SET NOT NULL;
ALTER TABLE memories
    ADD CONSTRAINT memories_pkey PRIMARY KEY (repo_id, snapshot_id);
ALTER TABLE memories
    ADD CONSTRAINT memories_snapshot_repo_fkey
    FOREIGN KEY (repo_id, snapshot_id) REFERENCES snapshots (repo_id, id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS memories_snapshot_idx ON memories (snapshot_id);
