LOCK TABLE blobs, blob_source_incarnations IN SHARE ROW EXCLUSIVE MODE;

CREATE SEQUENCE memory_read_object_wake_fence_seq AS bigint;

CREATE TABLE memory_read_object_wakes (
    hash text COLLATE "C" PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
    version bigint NOT NULL DEFAULT nextval('memory_read_object_wake_fence_seq') CHECK (version>0),
    after_repo text COLLATE "C" NOT NULL DEFAULT '' CHECK (octet_length(after_repo)<=71),
    next_attempt timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(next_attempt)),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('','temporary_failure','invalid_source'))
);

CREATE INDEX memory_read_object_wakes_due ON memory_read_object_wakes(next_attempt,hash);

CREATE OR REPLACE FUNCTION cxt_memory_read_object_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE affected_hash text;
BEGIN
    IF TG_OP='DELETE' THEN
        affected_hash=OLD.hash;
        IF NOT EXISTS(SELECT 1 FROM blobs WHERE hash=affected_hash) THEN RETURN NULL; END IF;
    ELSE
        affected_hash=NEW.hash;
    END IF;
    INSERT INTO memory_read_object_wakes(hash) VALUES(affected_hash)
    ON CONFLICT(hash) DO UPDATE SET version=nextval('memory_read_object_wake_fence_seq'),after_repo='',
        next_attempt=clock_timestamp(),attempts=0,reason='';
    RETURN NULL;
END
$$;

DROP TRIGGER memory_read_wake_object ON blobs;
CREATE TRIGGER memory_read_wake_object AFTER UPDATE ON blobs
FOR EACH ROW WHEN (OLD.doc_range_source_version IS DISTINCT FROM NEW.doc_range_source_version)
EXECUTE FUNCTION cxt_memory_read_object_wake();

CREATE OR REPLACE FUNCTION cxt_initialize_blob_incarnation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO blob_source_incarnations(hash) VALUES(NEW.hash);
    DELETE FROM memory_read_object_wakes WHERE hash=NEW.hash;
    RETURN NULL;
END
$$;
