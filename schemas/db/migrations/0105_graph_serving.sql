CREATE SEQUENCE graph_serving_fence AS bigint MINVALUE 1 NO CYCLE;

CREATE TABLE graph_serving_heads (
    repo_id text PRIMARY KEY REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_epoch uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    rules integer NOT NULL CHECK (rules > 0),
    comparison_rules integer NOT NULL CHECK (comparison_rules > 0),
    serving_fence bigint NOT NULL DEFAULT nextval('graph_serving_fence') CHECK (serving_fence > 0),
    expected_hash text NOT NULL CHECK (expected_hash ~ '^sha256:[0-9a-f]{64}$'),
    actual_hash text NOT NULL CHECK (actual_hash ~ '^sha256:[0-9a-f]{64}$'),
    completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (expected_hash = actual_hash),
    UNIQUE (repo_id, dataset, source_seq)
);

CREATE TABLE graph_serving_parts (
    repo_id text NOT NULL,
    dataset uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    kind text COLLATE "C" NOT NULL CHECK (octet_length(kind) BETWEEN 1 AND 128),
    entity_key text COLLATE "C" NOT NULL CHECK (octet_length(entity_key) BETWEEN 1 AND 4096),
    deleted boolean NOT NULL DEFAULT false CHECK (NOT deleted),
    image jsonb NOT NULL CHECK (image <> 'null'::jsonb),
    PRIMARY KEY (repo_id, kind, entity_key),
    FOREIGN KEY (repo_id, dataset, source_seq) REFERENCES graph_serving_heads(repo_id, dataset, source_seq) ON DELETE CASCADE
);

CREATE TABLE graph_serving_repairs (
    LIKE graph_projection_repairs INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES,
    serving_fence bigint NOT NULL CHECK (serving_fence > 0),
    FOREIGN KEY (repo_id) REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE
);
