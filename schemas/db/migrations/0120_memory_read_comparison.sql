CREATE TABLE memory_read_comparisons (
    repo_id text NOT NULL,
    read_id text COLLATE "C" NOT NULL CHECK (read_id ~ '^sha256:[0-9a-f]{64}$'),
    body bytea NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 4096),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repo_id, read_id),
    FOREIGN KEY (repo_id, read_id) REFERENCES memory_read_headers(repo_id, read_id) ON DELETE CASCADE
);

CREATE TRIGGER memory_read_comparison_immutable BEFORE UPDATE ON memory_read_comparisons
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();

CREATE FUNCTION cxt_memory_read_comparison_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    comparison jsonb;
    result_header jsonb;
    result_bytes bytea;
    target_bytes bytea;
    target jsonb;
BEGIN
    comparison := convert_from(NEW.body,'UTF8')::jsonb;
    SELECT header INTO result_bytes FROM memory_read_headers WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id;
    result_header := convert_from(result_bytes,'UTF8')::jsonb;
    SELECT work.target INTO target_bytes FROM memory_read_work work
        WHERE work.repo_id=NEW.repo_id AND work.target_id=comparison->>'target_id';
    target := convert_from(target_bytes,'UTF8')::jsonb;
    IF result_bytes IS NULL OR target_bytes IS NULL
        OR comparison->>'version' IS DISTINCT FROM '1'
        OR comparison->>'read_id' IS DISTINCT FROM NEW.read_id
        OR 'sha256:' || encode(sha256(result_bytes),'hex') IS DISTINCT FROM NEW.read_id
        OR comparison->>'capture_id' IS DISTINCT FROM result_header->>'capture_id'
        OR comparison->'source' IS DISTINCT FROM result_header->'source'
        OR comparison->'source'->'graph'->>'repo_id' IS DISTINCT FROM NEW.repo_id
        OR comparison->>'target_id' IS DISTINCT FROM 'sha256:' || encode(sha256(
            convert_to('cxthub:memory-read-target:v1','UTF8') || decode('00','hex') || target_bytes),'hex')
        OR target->>'version' IS DISTINCT FROM '1'
        OR target->>'repo_id' IS DISTINCT FROM NEW.repo_id
        OR target->>'rules' IS DISTINCT FROM result_header->>'rules'
        OR target->'selection' IS DISTINCT FROM result_header->'view'->'selection'
        OR COALESCE(target->>'content','') IS DISTINCT FROM COALESCE(result_header->'view'->>'content','')
        OR COALESCE(comparison->>'expected','') !~ '^sha256:[0-9a-f]{64}$'
        OR COALESCE(comparison->>'actual','') !~ '^sha256:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'memory comparison association mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_read_comparison_binding BEFORE INSERT ON memory_read_comparisons
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_comparison_binding();

CREATE FUNCTION cxt_memory_read_comparison_live() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM memory_read_work work
        JOIN memory_read_work_scopes scope USING(repo_id)
        WHERE work.repo_id=NEW.repo_id AND work.target_id=convert_from(NEW.body,'UTF8')::jsonb->>'target_id'
        AND scope.enabled AND work.state='running' AND work.lease_until>clock_timestamp()) THEN
        RAISE EXCEPTION 'memory comparison work expired' USING ERRCODE='23514', CONSTRAINT='memory_read_work_live';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM memory_read_headers result
        JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
        LEFT JOIN memory_capture_headers anchor ON anchor.repo_id=capture.repo_id AND anchor.capture_id=capture.graph_anchor_id
        WHERE result.repo_id=NEW.repo_id AND result.read_id=NEW.read_id
        AND capture.complete AND capture.expires_at>clock_timestamp()
        AND (capture.graph_anchor_id IS NULL OR (anchor.complete AND anchor.expires_at>clock_timestamp()))) THEN
        RAISE EXCEPTION 'memory comparison input expired' USING ERRCODE='23514', CONSTRAINT='memory_read_work_capture_live';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_read_comparison_live AFTER INSERT ON memory_read_comparisons
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_comparison_live();

ALTER TABLE memory_read_work DROP CONSTRAINT memory_read_work_reason_check;
ALTER TABLE memory_read_work ADD CONSTRAINT memory_read_work_reason_check CHECK (reason IN (
    '','temporary_failure','source_changed','source_reset','resource_limit','invalid_source','access_denied',
    'identity_upgrade_required','retention_expired','retention_cleanup','attempts_exhausted','comparison_mismatch'));
