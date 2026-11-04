CREATE OR REPLACE FUNCTION cxt_memory_read_qualification_live() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM memory_read_qualifications WHERE repo_id=NEW.repo_id AND qualification_id=NEW.qualification_id)
        AND NOT EXISTS (SELECT 1 FROM memory_read_headers result
            JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
            WHERE result.repo_id=NEW.repo_id AND result.read_id=NEW.read_id
            AND capture.complete AND capture.expires_at>clock_timestamp()) THEN
        RAISE EXCEPTION 'memory qualification input expired' USING ERRCODE='23514', CONSTRAINT='memory_read_qualification_capture_live';
    END IF;
    RETURN NULL;
END
$$;
