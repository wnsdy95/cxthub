CREATE TABLE graph_projection_work (
    LIKE pending_projection_work INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES,
    FOREIGN KEY (repo_id) REFERENCES repos(id) ON DELETE CASCADE
);

CREATE TABLE graph_projection_inputs (
    repo_id text NOT NULL REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    kind text COLLATE "C" NOT NULL CHECK (kind IN (
        'snapshot','ref','history','protocol','repository','pending','unsync','session_archive','reflog','revision',
        'git_commit','git_tree','git_delta','git_observation','pr_checkpoint','pr_participation',
        'pr_contribution','git_contribution','git_receipt','git_admission','git_context_head','git_contribution_proof')),
    entity_key text COLLATE "C" NOT NULL CHECK (octet_length(entity_key) BETWEEN 1 AND 4096),
    deleted boolean NOT NULL,
    image jsonb,
    PRIMARY KEY (repo_id, dataset, source_seq, kind, entity_key),
    CHECK ((deleted AND image IS NULL) OR (NOT deleted AND image IS NOT NULL AND jsonb_typeof(image) = 'object'))
);

CREATE INDEX graph_projection_inputs_latest ON graph_projection_inputs
    (repo_id, dataset, kind, entity_key, source_seq DESC);

CREATE TABLE graph_projection_parts (
    LIKE pending_projection_parts INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES,
    FOREIGN KEY (repo_id) REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE
);

CREATE TABLE graph_projection_ready (
    LIKE pending_projection_ready INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES,
    FOREIGN KEY (repo_id) REFERENCES graph_projection_work(repo_id) ON DELETE CASCADE
);

ALTER FUNCTION cxt_prune_catalog(text,bigint) RENAME TO cxt_prune_graph_source_catalog;

CREATE FUNCTION cxt_prune_catalog(p_repo text,p_floor bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE state repository_catalog_state;
BEGIN
    SELECT * INTO STRICT state FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF EXISTS (
        SELECT 1 FROM graph_projection_work
        WHERE repo_id=p_repo AND enabled AND source_epoch=state.epoch
          AND retention_until>clock_timestamp() AND pin_seq<p_floor
    ) THEN
        RAISE EXCEPTION 'catalog retention floor exceeds graph projection pin' USING ERRCODE='23514';
    END IF;
    PERFORM cxt_prune_graph_source_catalog(p_repo,p_floor);
END
$$;
