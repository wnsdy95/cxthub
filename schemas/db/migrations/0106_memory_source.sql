LOCK TABLE repos, repo_blobs, memories, pr_contribution_memories,
    git_contribution_memories IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE repository_memory_source_state (
    repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    epoch uuid NOT NULL DEFAULT gen_random_uuid(),
    head_seq bigint NOT NULL DEFAULT 0 CHECK (head_seq >= 0),
    writer_xid xid8
);

INSERT INTO repository_memory_source_state(repo_id) SELECT id FROM repos;

CREATE FUNCTION cxt_initialize_memory_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_memory_source_state(repo_id) VALUES(NEW.id);
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_source_repository AFTER INSERT ON repos
FOR EACH ROW EXECUTE FUNCTION cxt_initialize_memory_source();

CREATE FUNCTION cxt_advance_memory_source(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    UPDATE repository_memory_source_state
    SET head_seq=head_seq+1,writer_xid=pg_current_xact_id()
    WHERE repo_id=p_repo AND writer_xid IS DISTINCT FROM pg_current_xact_id();
    IF NOT FOUND AND NOT EXISTS (
        SELECT 1 FROM repository_memory_source_state
        WHERE repo_id=p_repo AND writer_xid=pg_current_xact_id()
    ) AND EXISTS (SELECT 1 FROM repos WHERE id=p_repo) THEN
        RAISE EXCEPTION 'missing repository memory source state' USING ERRCODE='23514';
    END IF;
END
$$;

CREATE FUNCTION cxt_capture_memory_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_row jsonb; after_row jsonb; affected_repo text;
BEGIN
    IF TG_OP <> 'INSERT' THEN before_row=to_jsonb(OLD)-'created_at'; END IF;
    IF TG_OP <> 'DELETE' THEN after_row=to_jsonb(NEW)-'created_at'; END IF;
    IF before_row IS NOT DISTINCT FROM after_row THEN RETURN NULL; END IF;
    FOR affected_repo IN
        SELECT DISTINCT owner FROM unnest(ARRAY[before_row->>'repo_id',after_row->>'repo_id']) AS owners(owner)
        WHERE owner IS NOT NULL ORDER BY owner
    LOOP
        PERFORM cxt_advance_memory_source(affected_repo);
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_source_legacy AFTER INSERT OR UPDATE OR DELETE ON memories
FOR EACH ROW EXECUTE FUNCTION cxt_capture_memory_metadata();
CREATE TRIGGER memory_source_pr_pins AFTER INSERT OR UPDATE OR DELETE ON pr_contribution_memories
FOR EACH ROW EXECUTE FUNCTION cxt_capture_memory_metadata();
CREATE TRIGGER memory_source_git_pins AFTER INSERT OR UPDATE OR DELETE ON git_contribution_memories
FOR EACH ROW EXECUTE FUNCTION cxt_capture_memory_metadata();

CREATE FUNCTION cxt_capture_memory_ownership() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_repo text; after_repo text; affected_repo text;
BEGIN
    IF TG_OP='UPDATE' AND (OLD.repo_id,OLD.kind,OLD.hash) IS NOT DISTINCT FROM (NEW.repo_id,NEW.kind,NEW.hash) THEN
        RETURN NULL;
    END IF;
    IF TG_OP <> 'INSERT' AND OLD.kind IN ('memory','memory_chunk') THEN before_repo=OLD.repo_id; END IF;
    IF TG_OP <> 'DELETE' AND NEW.kind IN ('memory','memory_chunk') THEN after_repo=NEW.repo_id; END IF;
    FOR affected_repo IN
        SELECT DISTINCT owner FROM unnest(ARRAY[before_repo,after_repo]) AS owners(owner)
        WHERE owner IS NOT NULL ORDER BY owner
    LOOP
        PERFORM cxt_advance_memory_source(affected_repo);
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_source_ownership_insert AFTER INSERT ON repo_blobs
FOR EACH ROW WHEN (NEW.kind IN ('memory','memory_chunk'))
EXECUTE FUNCTION cxt_capture_memory_ownership();
CREATE TRIGGER memory_source_ownership_update AFTER UPDATE ON repo_blobs
FOR EACH ROW WHEN (OLD.kind IN ('memory','memory_chunk') OR NEW.kind IN ('memory','memory_chunk'))
EXECUTE FUNCTION cxt_capture_memory_ownership();
CREATE TRIGGER memory_source_ownership_delete AFTER DELETE ON repo_blobs
FOR EACH ROW WHEN (OLD.kind IN ('memory','memory_chunk'))
EXECUTE FUNCTION cxt_capture_memory_ownership();

CREATE TRIGGER memory_source_legacy_truncate BEFORE TRUNCATE ON memories
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER memory_source_pr_pins_truncate BEFORE TRUNCATE ON pr_contribution_memories
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER memory_source_git_pins_truncate BEFORE TRUNCATE ON git_contribution_memories
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER memory_source_ownership_truncate BEFORE TRUNCATE ON repo_blobs
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
