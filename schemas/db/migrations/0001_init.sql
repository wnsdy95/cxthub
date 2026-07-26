-- cxt PostgreSQL DDL — canonical migration 0001
-- Apply order: psql -f 0001_init.sql
-- Initial content-addressed repository schema.
-- All content hashes are in 'sha256:<hex64>' TEXT format.
-- Immutable objects (snapshots, docs, blobs) have no delete API; garbage collection is an internal server policy.

-- ---------------------------------------------------------------------------
-- Extensions
-- ---------------------------------------------------------------------------
-- uuid_generate_v4() is used for team_identities convenience only.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ---------------------------------------------------------------------------
-- repos
-- ---------------------------------------------------------------------------
-- cxt manages git repositories. Multi-tenancy boundary = repo + team.
-- id = normalized remote URL or sha256 hash of cwd fallback.
CREATE TABLE IF NOT EXISTS repos (
    id              TEXT        PRIMARY KEY,              -- ContentHash (sha256:<hex>)
    remote_url      TEXT        NOT NULL,                 -- Normalized git remote URL
    default_branch  TEXT        NOT NULL DEFAULT 'main',
    team            TEXT        NOT NULL,                 -- Owning team (Team Token ownership)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  repos IS 'cxt manages repo metadata. id = sha256(normalized remote URL). Team unit visibility boundary.';
COMMENT ON COLUMN repos.id IS 'ContentHash. SHA-256 of the normalized remote URL. Local fallback is a hash of the working directory.';
COMMENT ON COLUMN repos.team IS 'Team name assigned from the Team Token during initial repository registration. Used for subsequent 403 authorization boundaries.';

CREATE INDEX IF NOT EXISTS repos_team_idx ON repos (team);

-- ---------------------------------------------------------------------------
-- blobs
-- ---------------------------------------------------------------------------
-- Content-addressed raw blob storage. Dedup unit for SessionDoc(CIR) body.
-- Hash is PRIMARY KEY → Duplicate storage of the same content is absolutely not allowed.
-- Snapshots/docs reference this table's hash.
CREATE TABLE IF NOT EXISTS blobs (
    hash        TEXT    PRIMARY KEY,                      -- ContentHash (sha256:<hex>)
    bytes       BYTEA   NOT NULL                          -- CIR JSON original (UTF-8)
);

COMMENT ON TABLE  blobs IS 'Content-addressed blob storage. hash = sha256(bytes). Dedup unit. No delete API.';
COMMENT ON COLUMN blobs.hash IS 'sha256:<hex64>. SHA-256 of bytes stored in blobs. Used for integrity check.';
COMMENT ON COLUMN blobs.bytes IS 'SessionDoc CIR JSON original. Server recalculates and verifies sha256(bytes)==hash upon receipt.';

-- ---------------------------------------------------------------------------
-- branches
-- ---------------------------------------------------------------------------
-- Convenience table. Instead of a view collecting only kind='branch' from refs, manage it as an explicit table.
-- Head pointer is the source of truth in refs table; here, only branch existence registration is managed.
CREATE TABLE IF NOT EXISTS branches (
    repo_id     TEXT        NOT NULL REFERENCES repos (id),
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, name)
);

COMMENT ON TABLE  branches IS 'Table for branch registration in repo. Head pointer source of truth is refs. Synchronized with refs.kind=branch.';
COMMENT ON COLUMN branches.name IS 'Branch name. Uses git branch name. Automatically generates <base>--fork-<shortid> form on fork.';

CREATE INDEX IF NOT EXISTS branches_repo_idx ON branches (repo_id);

-- ---------------------------------------------------------------------------
-- snapshots
-- ---------------------------------------------------------------------------
-- Immutable commit metadata. id = content hash (SessionDoc CIR's sha256).
-- The body(bytes) references the blobs table. Snapshots are immutable and cannot be modified or deleted once saved.
CREATE TABLE IF NOT EXISTS snapshots (
    id            TEXT        PRIMARY KEY,                -- ContentHash; same value as blobs.hash
    repo_id       TEXT        NOT NULL REFERENCES repos (id),
    branch        TEXT        NOT NULL,
    parents       TEXT[]      NOT NULL DEFAULT '{}',      -- Array of parent snapshot ids (DAG)
    doc_hash      TEXT        NOT NULL REFERENCES blobs (hash),  -- SessionDoc body hash
    memory_hash   TEXT        REFERENCES blobs (hash),   -- MemoryDigest blob hash (nullable)
    provider      TEXT        NOT NULL CHECK (provider IN ('claude', 'codex', 'unknown')),
    fidelity      TEXT        NOT NULL CHECK (fidelity IN ('full', 'reconstructed', 'memory')),
    message       TEXT        NOT NULL DEFAULT '',
    author_name   TEXT        NOT NULL DEFAULT '',
    author_email  TEXT        NOT NULL DEFAULT '',
    author_team   TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  snapshots IS 'Immutable commit metadata. id = sha256(SessionDoc CIR). Cannot be modified or deleted. DAG structure is represented by parents[].';
COMMENT ON COLUMN snapshots.id IS 'ContentHash. Snapshot.ID = ContentHash(canonical_bytes(SessionDoc.CIR)).';
COMMENT ON COLUMN snapshots.parents IS 'Array of parent snapshot ids. Empty array for DAG root. Used for ancestor reachability search in ff determination.';
COMMENT ON COLUMN snapshots.doc_hash IS 'References blobs.hash. SessionDoc CIR body.';
COMMENT ON COLUMN snapshots.memory_hash IS 'References blobs.hash. 1:1 with memories table. nullable (NULL if no memory).';
COMMENT ON COLUMN snapshots.fidelity IS 'full=original lossless | reconstructed=cross-reconstructed | memory=summary of inference.';

CREATE INDEX IF NOT EXISTS snapshots_repo_branch_idx  ON snapshots (repo_id, branch);
CREATE INDEX IF NOT EXISTS snapshots_repo_idx         ON snapshots (repo_id);
CREATE INDEX IF NOT EXISTS snapshots_doc_hash_idx     ON snapshots (doc_hash);
CREATE INDEX IF NOT EXISTS snapshots_created_at_idx   ON snapshots (created_at DESC);

-- ---------------------------------------------------------------------------
-- refs
-- ---------------------------------------------------------------------------
-- Mutable pointer (HEAD/branch/tag). Objects (snapshots/blobs) are immutable, but refs can be moved.
-- compare-and-swap(CAS) move: PUT /refs/{kind}/{name} implemented with version increment approach.
CREATE TABLE IF NOT EXISTS refs (
    repo_id         TEXT        NOT NULL REFERENCES repos (id),
    kind            TEXT        NOT NULL CHECK (kind IN ('head', 'branch', 'tag')),
    name            TEXT        NOT NULL,
    target          TEXT        NOT NULL REFERENCES snapshots (id),
    symbolic        TEXT        NOT NULL DEFAULT '',       -- HEAD points to a branch when branch name
    version         BIGINT      NOT NULL DEFAULT 1,        -- Optimistic CAS version
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, kind, name)
);

COMMENT ON TABLE  refs IS 'Variable pointer. Only refs can move; snapshots/blobs are immutable. CAS move: version comparison then UPDATE.';
COMMENT ON COLUMN refs.kind IS 'head=Current checkout symbolic ref(name=HEAD fixed) | branch | tag.';
COMMENT ON COLUMN refs.target IS 'Snapshot.id that refs points to. Object-priority invariant: target snapshot must exist before ref can move.';
COMMENT ON COLUMN refs.symbolic IS 'HEAD exclusive. Branch name it points to. Directly pointing to a snapshot (detached) is an empty string.';
COMMENT ON COLUMN refs.version IS 'Optimistic lock version. CAS: UPDATE ... WHERE version = $expected AND version = version + 1.';

CREATE INDEX IF NOT EXISTS refs_repo_idx ON refs (repo_id);

-- ---------------------------------------------------------------------------
-- memories
-- ---------------------------------------------------------------------------
-- Metadata for MemoryDigest attached to snapshots. Body references blobs(snapshots.memory_hash).
-- Here, only structured fields are stored (for full-text search/query optimization).
CREATE TABLE IF NOT EXISTS memories (
    snapshot_id TEXT        PRIMARY KEY REFERENCES snapshots (id),
    summary     TEXT        NOT NULL DEFAULT '',
    key_facts   TEXT[]      NOT NULL DEFAULT '{}',
    open_tasks  TEXT[]      NOT NULL DEFAULT '{}',
    provider    TEXT        NOT NULL CHECK (provider IN ('claude', 'codex', 'unknown')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  memories IS 'MemoryDigest structured metadata. 1:1 attachment to snapshots. Body blob is snapshots.memory_hash → blobs.';
COMMENT ON COLUMN memories.key_facts IS 'Array of key facts from the structured distillation output.';
COMMENT ON COLUMN memories.open_tasks IS 'Array of incomplete tasks. Used for context restoration in the next session.';

-- ---------------------------------------------------------------------------
-- team_identities
-- ---------------------------------------------------------------------------
-- Cache for team member identifiers. Snapshot.author ownership and audit log role.
-- Not used for access control; authorization is enforced separately.
CREATE TABLE IF NOT EXISTS team_identities (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    team        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    email       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team, email)
);

COMMENT ON TABLE team_identities IS 'Cache of team member identities for author attribution and audit logs; not used for access control.';
COMMENT ON COLUMN team_identities.email IS 'Upsert from X-Cxt-Identity header during push. team+email unique.';

CREATE INDEX IF NOT EXISTS team_identities_team_idx ON team_identities (team);
