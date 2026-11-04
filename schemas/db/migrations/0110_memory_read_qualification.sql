CREATE TABLE memory_read_qualifications (
    repo_id text NOT NULL,
    qualification_id text COLLATE "C" NOT NULL CHECK (qualification_id ~ '^sha256:[0-9a-f]{64}$'),
    read_id text COLLATE "C" NOT NULL CHECK (read_id ~ '^sha256:[0-9a-f]{64}$'),
    body bytea NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 2097152),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repo_id, qualification_id),
    FOREIGN KEY (repo_id, read_id) REFERENCES memory_read_headers(repo_id, read_id) ON DELETE CASCADE
);

CREATE INDEX memory_read_qualification_results ON memory_read_qualifications(repo_id, read_id);

CREATE TRIGGER memory_read_qualification_immutable BEFORE UPDATE ON memory_read_qualifications
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();

CREATE FUNCTION cxt_memory_read_qualification_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    qualification jsonb;
    result_header jsonb;
BEGIN
    qualification := convert_from(NEW.body,'UTF8')::jsonb;
    SELECT convert_from(header,'UTF8')::jsonb INTO result_header
        FROM memory_read_headers WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id;
    IF qualification->>'version' IS DISTINCT FROM '1'
        OR qualification->>'read_id' IS DISTINCT FROM NEW.read_id
        OR qualification->>'capture_id' IS DISTINCT FROM result_header->>'capture_id'
        OR qualification->'source' IS DISTINCT FROM result_header->'source'
        OR qualification->'source'->'graph'->>'repo_id' IS DISTINCT FROM NEW.repo_id
        OR 'sha256:' || encode(sha256(NEW.body),'hex') IS DISTINCT FROM NEW.qualification_id THEN
        RAISE EXCEPTION 'memory qualification association mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_read_qualification_binding BEFORE INSERT ON memory_read_qualifications
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_qualification_binding();

CREATE FUNCTION cxt_memory_read_qualification_live() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM memory_read_qualifications WHERE repo_id=NEW.repo_id AND qualification_id=NEW.qualification_id)
        AND NOT EXISTS (SELECT 1 FROM memory_read_headers result
            JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
            WHERE result.repo_id=NEW.repo_id AND result.read_id=NEW.read_id
            AND capture.complete AND capture.expires_at>clock_timestamp()) THEN
        RAISE EXCEPTION 'memory qualification input expired' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_read_qualification_live AFTER INSERT ON memory_read_qualifications
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_qualification_live();
