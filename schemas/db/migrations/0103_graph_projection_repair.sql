CREATE TABLE graph_projection_repairs (
    repo_id text PRIMARY KEY REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_epoch uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    rules integer NOT NULL CHECK (rules > 0),
    reason text NOT NULL CHECK (reason IN ('integrity','validation','resource_limit')),
    request_count bigint NOT NULL CHECK (request_count > 0),
    last_requested_at timestamptz NOT NULL,
    window_started_at timestamptz NOT NULL,
    rebuilds timestamptz[] NOT NULL CHECK (
        cardinality(rebuilds) BETWEEN 1 AND 3 AND
        array_ndims(rebuilds) = 1 AND array_lower(rebuilds,1) = 1 AND
        array_position(rebuilds,NULL) IS NULL
    ),
    quarantined boolean NOT NULL DEFAULT false
);
