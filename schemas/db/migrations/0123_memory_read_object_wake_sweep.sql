SET LOCAL lock_timeout='250ms';
SET LOCAL statement_timeout='5s';

ALTER TABLE memory_read_object_wakes ADD COLUMN scan_version bigint NOT NULL DEFAULT 0
    CHECK (scan_version>=0 AND scan_version<=version),
    ADD COLUMN through_repo text COLLATE "C" CHECK (octet_length(through_repo)<=71);

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
    ON CONFLICT(hash) DO UPDATE SET version=nextval('memory_read_object_wake_fence_seq'),
        scan_version=CASE WHEN memory_read_object_wakes.scan_version=0 THEN memory_read_object_wakes.version
            ELSE memory_read_object_wakes.scan_version END,
        next_attempt=LEAST(memory_read_object_wakes.next_attempt,clock_timestamp()),attempts=0,reason='';
    RETURN NULL;
END
$$;
