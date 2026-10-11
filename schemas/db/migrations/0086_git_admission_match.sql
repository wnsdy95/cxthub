CREATE TABLE git_admission_match (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> ''),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    provider_repository_id BIGINT NOT NULL CHECK (provider_repository_id > 0),
    page BIGINT NOT NULL CHECK (page > 0),
    version BIGINT NOT NULL CHECK (version > 0),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, origin, installation_id, provider_repository_id, page)
);
