CREATE SEQUENCE memory_read_work_fence_seq AS bigint;

CREATE TABLE memory_read_work_scopes (
    repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT false
);

CREATE TABLE memory_read_work (
    repo_id text NOT NULL REFERENCES memory_read_work_scopes(repo_id) ON DELETE CASCADE,
    target_id text COLLATE "C" NOT NULL CHECK (target_id ~ '^sha256:[0-9a-f]{64}$'),
    target bytea NOT NULL CHECK (octet_length(target) BETWEEN 1 AND 16384),
    source bytea NOT NULL CHECK (octet_length(source) BETWEEN 1 AND 1024),
    state text NOT NULL CHECK (state IN ('waiting','running','retrying','completed','rejected')),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('','temporary_failure','source_changed','source_reset','resource_limit','invalid_source','access_denied','identity_upgrade_required','retention_expired','retention_cleanup','attempts_exhausted')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
    version bigint NOT NULL DEFAULT nextval('memory_read_work_fence_seq') CHECK (version>0),
    qualification_id text COLLATE "C",
    created_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(created_at)),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(updated_at)),
    next_attempt timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(next_attempt)),
    lease_until timestamptz,
    PRIMARY KEY (repo_id,target_id),
    FOREIGN KEY (repo_id,qualification_id) REFERENCES memory_read_qualifications(repo_id,qualification_id) ON DELETE SET NULL (qualification_id),
    CHECK ((state='running' AND lease_until IS NOT NULL AND isfinite(lease_until) AND attempts>0)
        OR (state<>'running' AND lease_until IS NULL))
);

CREATE INDEX memory_read_work_due ON memory_read_work(repo_id,next_attempt,created_at,target_id)
    WHERE state IN ('waiting','running','retrying');
CREATE INDEX memory_read_work_active ON memory_read_work(repo_id)
    WHERE state IN ('waiting','running','retrying');
CREATE INDEX memory_read_work_terminal ON memory_read_work(repo_id,updated_at,target_id)
    WHERE state IN ('completed','rejected');
CREATE INDEX memory_read_work_qualification ON memory_read_work(repo_id,qualification_id)
    WHERE qualification_id IS NOT NULL;

CREATE FUNCTION cxt_memory_read_work_live() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state='completed' AND OLD.state='running' AND OLD.lease_until<=clock_timestamp() THEN
        RAISE EXCEPTION 'memory read work lease expired' USING ERRCODE='23514',CONSTRAINT='memory_read_work_live';
    END IF;
    IF NEW.state='completed' AND NEW.qualification_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM memory_read_qualifications qualification
        JOIN memory_read_headers result ON result.repo_id=qualification.repo_id AND result.read_id=qualification.read_id
        JOIN memory_capture_headers capture ON capture.repo_id=result.repo_id AND capture.capture_id=result.capture_id
        LEFT JOIN memory_capture_headers anchor ON anchor.repo_id=capture.repo_id AND anchor.capture_id=capture.graph_anchor_id
        WHERE qualification.repo_id=NEW.repo_id AND qualification.qualification_id=NEW.qualification_id
        AND capture.complete AND capture.expires_at>clock_timestamp()
        AND (capture.graph_anchor_id IS NULL OR (anchor.complete AND anchor.expires_at>clock_timestamp()))) THEN
        RAISE EXCEPTION 'memory read work capture expired' USING ERRCODE='23514',CONSTRAINT='memory_read_work_capture_live';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_read_work_live AFTER UPDATE ON memory_read_work
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_work_live();
