CREATE TABLE git_contributions (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, id)
);

CREATE TABLE git_contribution_memories (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    contribution_id TEXT NOT NULL CHECK (contribution_id ~ '^[0-9a-f]{64}$'),
    publication_id TEXT NOT NULL CHECK (publication_id ~ '^[0-9a-f]{32}$'),
    memory_hash TEXT NOT NULL CHECK (memory_hash = '' OR memory_hash ~ '^sha256:[0-9a-f]{64}$'),
    memory_source TEXT NOT NULL CHECK (memory_source = '' OR memory_source ~ '^sha256:[0-9a-f]{64}$'),
    memory_pinned BOOLEAN NOT NULL CHECK (memory_pinned),
    payload JSONB NOT NULL,
    CHECK (memory_hash = '' OR memory_source <> ''),
    PRIMARY KEY (repo_id, id),
    FOREIGN KEY (repo_id, contribution_id) REFERENCES git_contributions(repo_id, id) ON DELETE CASCADE
);

CREATE INDEX git_contribution_memories_contribution ON git_contribution_memories (repo_id, contribution_id, id);
