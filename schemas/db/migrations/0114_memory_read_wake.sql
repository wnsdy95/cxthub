LOCK TABLE repository_catalog_state, repository_memory_source_state,
    repository_view_source_state, repository_graph_source_state,
    memory_read_work_scopes, memory_read_work IN SHARE ROW EXCLUSIVE MODE;

CREATE SEQUENCE memory_read_wake_seq AS bigint;

CREATE TABLE memory_read_wakes (
    repo_id text PRIMARY KEY REFERENCES memory_read_work_scopes(repo_id) ON DELETE CASCADE,
    revision bigint NOT NULL CHECK (revision>0),
    object_revision bigint NOT NULL DEFAULT 0 CHECK (object_revision>=0 AND object_revision<=revision),
    writer_xid xid8,
    next_attempt timestamptz CHECK (next_attempt IS NULL OR isfinite(next_attempt)),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('','temporary_failure','source_reset','invalid_source','resource_limit','identity_upgrade_required','attempts_exhausted'))
);

CREATE INDEX memory_read_wakes_due ON memory_read_wakes(next_attempt,repo_id)
    WHERE next_attempt IS NOT NULL;

ALTER TABLE memory_read_work ADD COLUMN wake_revision bigint NOT NULL DEFAULT 0 CHECK (wake_revision>=0);

CREATE FUNCTION cxt_notify_memory_read_wake(p_repo text,p_objects boolean) RETURNS void LANGUAGE plpgsql AS $$
DECLARE stamp bigint;
BEGIN
    IF NOT EXISTS(SELECT 1 FROM memory_read_work_scopes WHERE repo_id=p_repo AND enabled) THEN RETURN; END IF;
    stamp=nextval('memory_read_wake_seq');
    INSERT INTO memory_read_wakes(repo_id,revision,object_revision,writer_xid,next_attempt)
    VALUES(p_repo,stamp,CASE WHEN p_objects THEN stamp ELSE 0 END,pg_current_xact_id(),clock_timestamp())
    ON CONFLICT(repo_id) DO UPDATE SET
        revision=CASE WHEN memory_read_wakes.writer_xid IS DISTINCT FROM pg_current_xact_id()
            THEN EXCLUDED.revision ELSE memory_read_wakes.revision END,
        object_revision=CASE WHEN p_objects THEN
            CASE WHEN memory_read_wakes.writer_xid IS DISTINCT FROM pg_current_xact_id()
                THEN EXCLUDED.revision ELSE memory_read_wakes.revision END
            ELSE memory_read_wakes.object_revision END,
        writer_xid=pg_current_xact_id(),next_attempt=clock_timestamp(),attempts=0,reason='';
END
$$;

CREATE FUNCTION cxt_schedule_memory_read_wake(p_repo text,p_due timestamptz) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF p_due IS NULL OR NOT EXISTS(SELECT 1 FROM memory_read_work_scopes WHERE repo_id=p_repo AND enabled) THEN RETURN; END IF;
    INSERT INTO memory_read_wakes(repo_id,revision,next_attempt)
    VALUES(p_repo,nextval('memory_read_wake_seq'),p_due)
    ON CONFLICT(repo_id) DO UPDATE SET next_attempt=LEAST(memory_read_wakes.next_attempt,EXCLUDED.next_attempt);
END
$$;

CREATE FUNCTION cxt_memory_read_source_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected_repo text;
BEGIN
    IF TG_OP='UPDATE' THEN
        IF TG_TABLE_NAME IN ('repository_catalog_state','repository_memory_source_state') THEN
            IF (OLD.epoch,OLD.head_seq) IS NOT DISTINCT FROM (NEW.epoch,NEW.head_seq) THEN RETURN NULL; END IF;
        ELSE
            IF (OLD.epoch,OLD.baseline_seq) IS NOT DISTINCT FROM (NEW.epoch,NEW.baseline_seq) THEN RETURN NULL; END IF;
        END IF;
    END IF;
    IF TG_OP='DELETE' THEN affected_repo=OLD.repo_id; ELSE affected_repo=NEW.repo_id; END IF;
    IF EXISTS(SELECT 1 FROM repos WHERE id=affected_repo) THEN
        PERFORM cxt_notify_memory_read_wake(affected_repo,false);
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_read_wake_catalog AFTER INSERT OR UPDATE OR DELETE ON repository_catalog_state
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_source_wake();
CREATE TRIGGER memory_read_wake_memory AFTER INSERT OR UPDATE OR DELETE ON repository_memory_source_state
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_source_wake();
CREATE TRIGGER memory_read_wake_view AFTER INSERT OR UPDATE OR DELETE ON repository_view_source_state
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_source_wake();
CREATE TRIGGER memory_read_wake_graph AFTER INSERT OR UPDATE OR DELETE ON repository_graph_source_state
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_source_wake();

CREATE FUNCTION cxt_memory_read_object_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_hash text; after_hash text; affected_repo text;
BEGIN
    IF TG_OP<>'INSERT' THEN before_hash=OLD.hash; END IF;
    IF TG_OP<>'DELETE' THEN after_hash=NEW.hash; END IF;
    FOR affected_repo IN
        SELECT DISTINCT owned.repo_id FROM repo_blobs owned
        JOIN memory_read_work_scopes scope ON scope.repo_id=owned.repo_id AND scope.enabled
        WHERE owned.hash=ANY(ARRAY[before_hash,after_hash]) AND owned.kind IN ('memory','memory_chunk')
        ORDER BY owned.repo_id
    LOOP
        PERFORM cxt_notify_memory_read_wake(affected_repo,true);
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_read_wake_object AFTER UPDATE OF bytes,doc_range_source_version ON blobs
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_object_wake();
CREATE TRIGGER memory_read_wake_incarnation AFTER INSERT OR DELETE ON blob_source_incarnations
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_object_wake();

CREATE FUNCTION cxt_memory_read_scope_wake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT NEW.enabled THEN
        UPDATE memory_read_wakes SET next_attempt=NULL WHERE repo_id=NEW.repo_id;
    ELSIF TG_OP='INSERT' OR NOT OLD.enabled THEN
        PERFORM cxt_notify_memory_read_wake(NEW.repo_id,true);
    ELSE
        PERFORM cxt_schedule_memory_read_wake(NEW.repo_id,clock_timestamp());
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_read_wake_scope AFTER INSERT OR UPDATE OF enabled ON memory_read_work_scopes
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_scope_wake();

CREATE FUNCTION cxt_memory_read_completed_wake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    SELECT COALESCE(max(revision),NEW.wake_revision) INTO NEW.wake_revision
    FROM memory_read_wakes WHERE repo_id=NEW.repo_id;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_read_completed_wake BEFORE UPDATE OF state ON memory_read_work
FOR EACH ROW WHEN (NEW.state='completed' AND OLD.state='running')
EXECUTE FUNCTION cxt_memory_read_completed_wake();

CREATE FUNCTION cxt_memory_read_result_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE due timestamptz;
BEGIN
    IF TG_OP='UPDATE' AND (OLD.state,OLD.qualification_id) IS NOT DISTINCT FROM (NEW.state,NEW.qualification_id) THEN RETURN NULL; END IF;
    IF NEW.state='completed' AND NEW.qualification_id IS NOT NULL THEN
        SELECT capture.expires_at INTO due FROM memory_read_qualifications qualified
        JOIN memory_read_headers result ON result.repo_id=qualified.repo_id AND result.read_id=qualified.read_id
        JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
        WHERE qualified.repo_id=NEW.repo_id AND qualified.qualification_id=NEW.qualification_id;
    ELSIF TG_OP='INSERT' OR NEW.state IN ('completed','rejected') OR
        (NEW.state='retrying' AND EXISTS(SELECT 1 FROM memory_read_wakes
            WHERE repo_id=NEW.repo_id AND revision>NEW.wake_revision)) THEN
        due=clock_timestamp();
    END IF;
    PERFORM cxt_schedule_memory_read_wake(NEW.repo_id,COALESCE(due,
        CASE WHEN NEW.state='completed' THEN clock_timestamp() END));
    RETURN NULL;
END
$$;

CREATE TRIGGER memory_read_wake_result AFTER INSERT OR UPDATE OF state,qualification_id ON memory_read_work
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_result_wake();

SELECT cxt_notify_memory_read_wake(repo_id,true) FROM memory_read_work_scopes WHERE enabled ORDER BY repo_id;
