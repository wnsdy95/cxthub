CREATE TABLE graph_projection_references (
    repo_id text PRIMARY KEY REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_epoch uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    rules integer NOT NULL CHECK (rules > 0),
    comparison_rules integer NOT NULL CHECK (comparison_rules > 0),
    expected_hash text NOT NULL CHECK (expected_hash ~ '^sha256:[0-9a-f]{64}$')
);

ALTER TABLE graph_qualification
    ADD COLUMN attempt_reference_hash text CHECK (attempt_reference_hash ~ '^sha256:[0-9a-f]{64}$'),
    ADD CONSTRAINT graph_qualification_reference_attempt CHECK (attempt_reference_hash IS NULL OR attempt_dataset IS NOT NULL);
