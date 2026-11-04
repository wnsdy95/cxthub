LOCK TABLE repos, snapshots, refs, context_history, pending_contexts, unsync_contexts, session_archives, reflog, repository_revisions, git_commit_trees, git_tree_nodes, git_commit_deltas, git_ref_observations, pr_history_checkpoints, pr_participations, pr_contributions, git_contributions, git_head_receipts, git_contribution_admissions, git_context_heads, git_contribution_proofs, repository_catalog_state,
    repository_view_source_state IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE repository_graph_source_state (
    repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    baseline_seq bigint NOT NULL CHECK (baseline_seq >= 0)
);

CREATE TABLE repository_graph_source_changes (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq >= 0),
    entity_kind text COLLATE "C" NOT NULL CHECK (entity_kind IN ('git_commit','git_tree','git_delta','git_observation','pr_checkpoint','pr_participation','pr_contribution','git_contribution','git_receipt','git_admission','git_context_head','git_contribution_proof')),
    entity_key text COLLATE "C" NOT NULL CHECK (entity_key <> ''),
    deleted boolean NOT NULL,
    after_image jsonb,
    PRIMARY KEY (repo_id,epoch,seq,entity_kind,entity_key),
    CHECK ((deleted AND after_image IS NULL) OR
        (NOT deleted AND after_image IS NOT NULL AND jsonb_typeof(after_image)='object'))
);

CREATE INDEX repository_graph_source_latest ON repository_graph_source_changes
    (repo_id,epoch,entity_kind,entity_key,seq DESC);

CREATE VIEW repository_graph_source_images AS
SELECT repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image FROM repository_view_source_images
UNION ALL
SELECT repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image FROM repository_graph_source_changes;

CREATE FUNCTION cxt_repository_graph_source_key(p_kind text,p_row jsonb)
RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    RETURN CASE p_kind
        WHEN 'git_commit' THEN jsonb_build_array(p_row->>'origin',p_row->>'commit_oid')::text
        WHEN 'git_tree' THEN jsonb_build_array(p_row->>'origin',p_row->>'oid')::text
        WHEN 'git_delta' THEN jsonb_build_array(p_row->>'origin',p_row->>'commit_oid',p_row->>'parent_oid')::text
        WHEN 'git_observation' THEN p_row->>'id'
        WHEN 'pr_checkpoint' THEN jsonb_build_array(p_row->>'origin',p_row->>'provider_repository_id',p_row->>'number')::text
        WHEN 'pr_participation' THEN p_row->>'id'
        WHEN 'pr_contribution' THEN p_row->>'id'
        WHEN 'git_contribution' THEN p_row->>'id'
        WHEN 'git_receipt' THEN jsonb_build_array(p_row->>'origin',p_row->>'version')::text
        WHEN 'git_admission' THEN p_row->>'id'
        WHEN 'git_context_head' THEN p_row->>'id'
        WHEN 'git_contribution_proof' THEN p_row->>'id'
        ELSE NULL END;
END
$$;

CREATE FUNCTION cxt_repository_graph_source_image(p_kind text,p_row jsonb)
RETURNS jsonb LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    RETURN CASE p_kind
        WHEN 'git_commit' THEN jsonb_build_object('repo_id',p_row->'repo_id','origin',p_row->'origin','commit_oid',p_row->'commit_oid','payload',p_row->'payload')
        WHEN 'git_tree' THEN jsonb_build_object('repo_id',p_row->'repo_id','origin',p_row->'origin','oid',p_row->'oid','payload',p_row->'payload')
        WHEN 'git_delta' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','origin',p_row->'origin','commit_oid',p_row->'commit_oid','parent_oid',p_row->'parent_oid','payload',p_row->'payload')
        WHEN 'git_observation' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','payload',p_row->'payload')
        WHEN 'pr_checkpoint' THEN jsonb_build_object('repo_id',p_row->'repo_id','origin',p_row->'origin','provider_repository_id',p_row->'provider_repository_id','number',p_row->'number','version',p_row->'version','payload',p_row->'payload')
        WHEN 'pr_participation' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','origin',p_row->'origin','provider_repository_id',p_row->'provider_repository_id','number',p_row->'number','payload',p_row->'payload')
        WHEN 'pr_contribution' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','payload',p_row->'payload')
        WHEN 'git_contribution' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','payload',p_row->'payload')
        WHEN 'git_receipt' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','origin',p_row->'origin','version',p_row->'version','payload',p_row->'payload')
        WHEN 'git_admission' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','admission_key',p_row->'admission_key','origin',p_row->'origin','receipt_id',p_row->'receipt_id','payload',p_row->'payload')
        WHEN 'git_context_head' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','origin',p_row->'origin','destination_branch_id',p_row->'destination_branch_id','destination_branch',p_row->'destination_branch','read_version',p_row->'read_version','receipt_id',p_row->'receipt_id','payload',p_row->'payload')
        WHEN 'git_contribution_proof' THEN jsonb_build_object('repo_id',p_row->'repo_id','id',p_row->'id','admission_id',p_row->'admission_id','context_head_id',p_row->'context_head_id','contribution_id',p_row->'contribution_id','payload',p_row->'payload')
        ELSE NULL END;
END
$$;

CREATE FUNCTION cxt_record_repository_graph_source(p_repo text,p_kind text,p_key text,p_deleted boolean,p_image jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE generation bigint; incarnation uuid;
BEGIN
    SELECT allocated.seq,allocated.epoch INTO generation,incarnation
    FROM cxt_catalog_generation(p_repo) AS allocated;
    IF NOT FOUND THEN RETURN; END IF;
    IF NOT EXISTS (SELECT 1 FROM repository_graph_source_state
        WHERE repo_id=p_repo AND epoch=incarnation AND baseline_seq<=generation) THEN
        RAISE EXCEPTION 'missing or mismatched repository graph source state' USING ERRCODE='23514';
    END IF;
    INSERT INTO repository_graph_source_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    VALUES(p_repo,incarnation,generation,p_kind,p_key,p_deleted,p_image)
    ON CONFLICT (repo_id,epoch,seq,entity_kind,entity_key)
    DO UPDATE SET deleted=EXCLUDED.deleted,after_image=EXCLUDED.after_image;
END
$$;

CREATE FUNCTION cxt_capture_repository_graph_source() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source_kind text := TG_ARGV[0]; before_row jsonb; after_row jsonb;
    before_key text; after_key text; before_image jsonb; after_image jsonb;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        before_row=to_jsonb(OLD);
        before_key=cxt_repository_graph_source_key(source_kind,before_row);
        before_image=cxt_repository_graph_source_image(source_kind,before_row);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        after_row=to_jsonb(NEW);
        after_key=cxt_repository_graph_source_key(source_kind,after_row);
        after_image=cxt_repository_graph_source_image(source_kind,after_row);
    END IF;
    IF TG_OP='UPDATE' AND before_row->>'repo_id' IS NOT DISTINCT FROM after_row->>'repo_id'
        AND before_key IS NOT DISTINCT FROM after_key AND before_image IS NOT DISTINCT FROM after_image THEN
        RETURN NULL;
    END IF;
    IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND
        (before_row->>'repo_id' IS DISTINCT FROM after_row->>'repo_id' OR before_key IS DISTINCT FROM after_key)) THEN
        PERFORM cxt_record_repository_graph_source(before_row->>'repo_id',source_kind,before_key,true,NULL);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM cxt_record_repository_graph_source(after_row->>'repo_id',source_kind,after_key,false,after_image);
    END IF;
    RETURN NULL;
END
$$;

CREATE FUNCTION cxt_seed_repository_graph_source(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE state repository_catalog_state;
BEGIN
    SELECT * INTO state FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM repos WHERE id=p_repo) THEN RETURN; END IF;
        RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM repository_graph_source_state
        WHERE repo_id=p_repo AND epoch=state.epoch AND baseline_seq=state.head_seq) THEN
        RAISE EXCEPTION 'missing or mismatched repository graph source baseline' USING ERRCODE='23514';
    END IF;
    INSERT INTO repository_graph_source_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    SELECT state.repo_id,state.epoch,state.head_seq,source.kind,
        cxt_repository_graph_source_key(source.kind,source.row),false,
        cxt_repository_graph_source_image(source.kind,source.row)
    FROM (
        SELECT 'git_commit' AS kind,to_jsonb(source) AS row FROM git_commit_trees source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_tree' AS kind,to_jsonb(source) AS row FROM git_tree_nodes source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_delta' AS kind,to_jsonb(source) AS row FROM git_commit_deltas source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_observation' AS kind,to_jsonb(source) AS row FROM git_ref_observations source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'pr_checkpoint' AS kind,to_jsonb(source) AS row FROM pr_history_checkpoints source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'pr_participation' AS kind,to_jsonb(source) AS row FROM pr_participations source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'pr_contribution' AS kind,to_jsonb(source) AS row FROM pr_contributions source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_contribution' AS kind,to_jsonb(source) AS row FROM git_contributions source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_receipt' AS kind,to_jsonb(source) AS row FROM git_head_receipts source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_admission' AS kind,to_jsonb(source) AS row FROM git_contribution_admissions source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_context_head' AS kind,to_jsonb(source) AS row FROM git_context_heads source WHERE source.repo_id=p_repo
        UNION ALL
        SELECT 'git_contribution_proof' AS kind,to_jsonb(source) AS row FROM git_contribution_proofs source WHERE source.repo_id=p_repo
    ) AS source;
END
$$;

CREATE FUNCTION cxt_initialize_repository_graph_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_graph_source_state(repo_id,epoch,baseline_seq)
    SELECT repo_id,epoch,head_seq FROM repository_catalog_state WHERE repo_id=NEW.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER catalog_repository_graph AFTER INSERT ON repos
FOR EACH ROW EXECUTE FUNCTION cxt_initialize_repository_graph_source();

CREATE TRIGGER catalog_graph_git_commit AFTER INSERT OR UPDATE OR DELETE ON git_commit_trees
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_commit');
CREATE TRIGGER catalog_graph_git_commit_truncate BEFORE TRUNCATE ON git_commit_trees
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_tree AFTER INSERT OR UPDATE OR DELETE ON git_tree_nodes
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_tree');
CREATE TRIGGER catalog_graph_git_tree_truncate BEFORE TRUNCATE ON git_tree_nodes
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_delta AFTER INSERT OR UPDATE OR DELETE ON git_commit_deltas
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_delta');
CREATE TRIGGER catalog_graph_git_delta_truncate BEFORE TRUNCATE ON git_commit_deltas
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_observation AFTER INSERT OR UPDATE OR DELETE ON git_ref_observations
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_observation');
CREATE TRIGGER catalog_graph_git_observation_truncate BEFORE TRUNCATE ON git_ref_observations
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_pr_checkpoint AFTER INSERT OR UPDATE OR DELETE ON pr_history_checkpoints
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('pr_checkpoint');
CREATE TRIGGER catalog_graph_pr_checkpoint_truncate BEFORE TRUNCATE ON pr_history_checkpoints
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_pr_participation AFTER INSERT OR UPDATE OR DELETE ON pr_participations
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('pr_participation');
CREATE TRIGGER catalog_graph_pr_participation_truncate BEFORE TRUNCATE ON pr_participations
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_pr_contribution AFTER INSERT OR UPDATE OR DELETE ON pr_contributions
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('pr_contribution');
CREATE TRIGGER catalog_graph_pr_contribution_truncate BEFORE TRUNCATE ON pr_contributions
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_contribution AFTER INSERT OR UPDATE OR DELETE ON git_contributions
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_contribution');
CREATE TRIGGER catalog_graph_git_contribution_truncate BEFORE TRUNCATE ON git_contributions
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_receipt AFTER INSERT OR UPDATE OR DELETE ON git_head_receipts
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_receipt');
CREATE TRIGGER catalog_graph_git_receipt_truncate BEFORE TRUNCATE ON git_head_receipts
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_admission AFTER INSERT OR UPDATE OR DELETE ON git_contribution_admissions
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_admission');
CREATE TRIGGER catalog_graph_git_admission_truncate BEFORE TRUNCATE ON git_contribution_admissions
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_context_head AFTER INSERT OR UPDATE OR DELETE ON git_context_heads
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_context_head');
CREATE TRIGGER catalog_graph_git_context_head_truncate BEFORE TRUNCATE ON git_context_heads
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_graph_git_contribution_proof AFTER INSERT OR UPDATE OR DELETE ON git_contribution_proofs
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_graph_source('git_contribution_proof');
CREATE TRIGGER catalog_graph_git_contribution_proof_truncate BEFORE TRUNCATE ON git_contribution_proofs
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();

ALTER FUNCTION cxt_reset_catalog(text) RENAME TO cxt_reset_repository_view_catalog;

CREATE FUNCTION cxt_reset_catalog(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE repos, snapshots, refs, context_history, pending_contexts, unsync_contexts, session_archives, reflog, repository_revisions, git_commit_trees, git_tree_nodes, git_commit_deltas, git_ref_observations, pr_history_checkpoints, pr_participations, pr_contributions, git_contributions, git_head_receipts, git_contribution_admissions, git_context_heads, git_contribution_proofs IN SHARE ROW EXCLUSIVE MODE;
    PERFORM cxt_reset_repository_view_catalog(p_repo);
    DELETE FROM repository_graph_source_changes WHERE repo_id=p_repo;
    INSERT INTO repository_graph_source_state(repo_id,epoch,baseline_seq)
    SELECT repo_id,epoch,head_seq FROM repository_catalog_state WHERE repo_id=p_repo
    ON CONFLICT (repo_id) DO UPDATE SET epoch=EXCLUDED.epoch,baseline_seq=EXCLUDED.baseline_seq;
    PERFORM cxt_seed_repository_graph_source(p_repo);
END
$$;

ALTER FUNCTION cxt_prune_catalog(text,bigint) RENAME TO cxt_prune_repository_view_catalog;

CREATE FUNCTION cxt_prune_catalog(p_repo text,p_floor bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE state repository_catalog_state;
BEGIN
    SELECT * INTO STRICT state FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF NOT EXISTS (SELECT 1 FROM repository_graph_source_state
        WHERE repo_id=p_repo AND epoch=state.epoch AND baseline_seq<=state.head_seq) THEN
        RAISE EXCEPTION 'missing or mismatched repository graph source state' USING ERRCODE='23514';
    END IF;
    PERFORM cxt_prune_repository_view_catalog(p_repo,p_floor);
    DELETE FROM repository_graph_source_changes old
    WHERE old.repo_id=p_repo AND old.epoch=state.epoch AND old.seq<p_floor
      AND EXISTS (SELECT 1 FROM repository_graph_source_changes newer
          WHERE newer.repo_id=old.repo_id AND newer.epoch=old.epoch
            AND newer.entity_kind=old.entity_kind AND newer.entity_key=old.entity_key
            AND newer.seq>old.seq AND newer.seq<=p_floor);
END
$$;

INSERT INTO repository_graph_source_state(repo_id,epoch,baseline_seq)
SELECT repo_id,epoch,head_seq FROM repository_catalog_state;
SELECT cxt_seed_repository_graph_source(repo_id) FROM repository_catalog_state;

COMMENT ON TABLE repository_graph_source_changes IS
    'Private fixed after-images for repository-graph-v1. Shares catalog commit ordering; never part of sync-metadata-v1 or repository-view-v1. Contains Git/PR proof metadata, not job state, authentication data, source file bodies or effective-memory/blob grants. Consumers still verify typed proofs, current authorization and identity; captured input is not a ready projection.';
COMMENT ON TABLE repository_graph_source_state IS
    'Graph input capture starts at this baseline in the shared catalog epoch. Read checkpoints must also respect the view baseline and catalog retention floor. Projection readiness and retention pins are separate.';
