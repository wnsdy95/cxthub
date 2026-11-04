DROP TRIGGER memory_read_completed_wake ON memory_read_work;
DROP FUNCTION cxt_memory_read_completed_wake();

CREATE OR REPLACE FUNCTION cxt_memory_read_result_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE due timestamptz;
BEGIN
    IF TG_OP='UPDATE' AND (OLD.state,OLD.qualification_id) IS NOT DISTINCT FROM (NEW.state,NEW.qualification_id) THEN RETURN NULL; END IF;
    IF NEW.state='completed' AND NEW.qualification_id IS NOT NULL THEN
        SELECT capture.expires_at INTO due FROM memory_read_qualifications qualified
        JOIN memory_read_headers result ON result.repo_id=qualified.repo_id AND result.read_id=qualified.read_id
        JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
        WHERE qualified.repo_id=NEW.repo_id AND qualified.qualification_id=NEW.qualification_id;
        IF EXISTS(SELECT 1 FROM memory_read_wakes WHERE repo_id=NEW.repo_id AND revision>NEW.wake_revision) THEN
            due=clock_timestamp();
        END IF;
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
