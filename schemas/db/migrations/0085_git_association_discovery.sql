CREATE TABLE git_association_discovery (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> ''),
    version BIGINT NOT NULL CHECK (version > 0),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, origin)
);

CREATE TABLE git_association_publications (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> ''),
    publication_id TEXT NOT NULL CHECK (publication_id ~ '^[0-9a-f]{32}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, origin, publication_id)
);
