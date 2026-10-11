CREATE TABLE pr_history_checkpoints (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> ''),
    provider_repository_id BIGINT NOT NULL CHECK (provider_repository_id > 0),
    number BIGINT NOT NULL CHECK (number > 0),
    version BIGINT NOT NULL CHECK (version > 0),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, origin, provider_repository_id, number)
);

CREATE TABLE pr_participations (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    origin TEXT NOT NULL CHECK (origin <> ''),
    provider_repository_id BIGINT NOT NULL CHECK (provider_repository_id > 0),
    number BIGINT NOT NULL CHECK (number > 0),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, id)
);

CREATE INDEX pr_participations_lookup ON pr_participations (repo_id, origin, number, id);

CREATE INDEX git_ref_observations_pr_history ON git_ref_observations (
    repo_id,
    (payload->>'git_origin'),
    (payload->'pull_request'->>'repository_id'),
    (payload->'pull_request'->>'number'),
    id
) WHERE payload->>'source' IN ('github-pull-request', 'github-pr-reconciliation');
