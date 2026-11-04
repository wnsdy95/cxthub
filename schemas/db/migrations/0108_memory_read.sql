DO $$
BEGIN
    IF to_regclass('memory_capture_headers') IS NULL
        OR to_regprocedure('cxt_memory_capture_immutable()') IS NULL THEN
        RAISE EXCEPTION '0108_memory_read requires 0107_memory_capture';
    END IF;
END
$$;

CREATE TABLE memory_read_headers (
    repo_id text NOT NULL,
    read_id text COLLATE "C" NOT NULL CHECK (read_id ~ '^sha256:[0-9a-f]{64}$'),
    capture_id text COLLATE "C" NOT NULL CHECK (capture_id ~ '^sha256:[0-9a-f]{64}$'),
    header bytea NOT NULL CHECK (octet_length(header) BETWEEN 1 AND 4194304),
    chunk_count integer NOT NULL CHECK (chunk_count BETWEEN 0 AND 1024),
    item_count integer NOT NULL CHECK (item_count BETWEEN 0 AND 16384),
    body_bytes bigint NOT NULL CHECK (body_bytes BETWEEN 0 AND 67108864),
    PRIMARY KEY (repo_id, read_id),
    FOREIGN KEY (repo_id, capture_id) REFERENCES memory_capture_headers(repo_id, capture_id) ON DELETE CASCADE,
    CHECK (body_bytes + octet_length(header) <= 67108864),
    CHECK ((chunk_count = 0) = (item_count = 0))
);

CREATE INDEX memory_read_capture_references ON memory_read_headers(repo_id, capture_id);

CREATE TABLE memory_read_chunks (
    repo_id text NOT NULL,
    read_id text COLLATE "C" NOT NULL CHECK (read_id ~ '^sha256:[0-9a-f]{64}$'),
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 1023),
    chunk_hash text COLLATE "C" NOT NULL CHECK (chunk_hash ~ '^sha256:[0-9a-f]{64}$'),
    start_item integer NOT NULL CHECK (start_item BETWEEN 0 AND 16383),
    item_count integer NOT NULL CHECK (item_count BETWEEN 1 AND 64),
    body bytea NOT NULL CHECK (octet_length(body) BETWEEN 2 AND 262146),
    PRIMARY KEY (repo_id, read_id, ordinal),
    FOREIGN KEY (repo_id, read_id) REFERENCES memory_read_headers(repo_id, read_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);

CREATE TRIGGER memory_read_header_immutable BEFORE UPDATE ON memory_read_headers
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();
CREATE TRIGGER memory_read_chunk_immutable BEFORE UPDATE ON memory_read_chunks
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();

CREATE FUNCTION cxt_memory_read_chunk_sealed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_repo text;
    target_read text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        target_repo := OLD.repo_id;
        target_read := OLD.read_id;
    ELSE
        target_repo := NEW.repo_id;
        target_read := NEW.read_id;
    END IF;
    IF EXISTS (SELECT 1 FROM memory_read_headers WHERE repo_id=target_repo AND read_id=target_read) THEN
        RAISE EXCEPTION 'memory read chunks are sealed' USING ERRCODE='23514';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_read_chunk_sealed BEFORE INSERT OR DELETE ON memory_read_chunks
FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_chunk_sealed();

CREATE FUNCTION cxt_memory_read_chunk_indexed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM memory_read_chunks WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id AND ordinal=NEW.ordinal)
        AND NOT EXISTS (SELECT 1 FROM memory_read_headers
            WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id AND NEW.ordinal<chunk_count) THEN
        RAISE EXCEPTION 'memory read chunk is not indexed' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_read_chunk_indexed AFTER INSERT ON memory_read_chunks
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_chunk_indexed();

CREATE FUNCTION cxt_memory_read_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    read_header jsonb;
    capture_header jsonb;
    actual_chunks bigint;
    actual_items bigint;
    actual_bytes bigint;
    invalid boolean;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM memory_read_headers WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id) THEN
        RETURN NULL;
    END IF;
    read_header := convert_from(NEW.header, 'UTF8')::jsonb;
    SELECT convert_from(header, 'UTF8')::jsonb INTO capture_header
        FROM memory_capture_headers WHERE repo_id=NEW.repo_id AND capture_id=NEW.capture_id
        AND complete AND expires_at>clock_timestamp() FOR KEY SHARE;
    IF capture_header IS NULL
        OR read_header->>'capture_id' IS DISTINCT FROM NEW.capture_id
        OR read_header->'source' IS DISTINCT FROM capture_header->'generation'
        OR read_header->'view'->'selection' IS DISTINCT FROM capture_header->'selection'
        OR COALESCE(read_header->'view'->>'content','') IS DISTINCT FROM COALESCE(capture_header->>'content','')
        OR read_header->'source'->'graph'->>'repo_id' IS DISTINCT FROM NEW.repo_id
        OR read_header->'view'->>'total' IS DISTINCT FROM NEW.item_count::text
        OR COALESCE(jsonb_array_length(NULLIF(read_header->'chunks','null'::jsonb)),0) IS DISTINCT FROM NEW.chunk_count
        OR 'sha256:' || encode(sha256(NEW.header),'hex') IS DISTINCT FROM NEW.read_id THEN
        RAISE EXCEPTION 'memory read header association mismatch' USING ERRCODE='23514';
    END IF;
    SELECT count(*),COALESCE(sum(item_count),0),COALESCE(sum(octet_length(body)),0),
        COALESCE(bool_or(ordinal<>expected_ordinal OR start_item<>expected_start
            OR read_header->'chunks'->ordinal->>'hash' IS DISTINCT FROM chunk_hash
            OR read_header->'chunks'->ordinal->>'start' IS DISTINCT FROM start_item::text
            OR read_header->'chunks'->ordinal->>'count' IS DISTINCT FROM item_count::text
            OR read_header->'chunks'->ordinal->>'bytes' IS DISTINCT FROM octet_length(body)::text),false)
        INTO actual_chunks,actual_items,actual_bytes,invalid
        FROM (SELECT *,row_number() OVER (ORDER BY ordinal)-1 AS expected_ordinal,
            COALESCE(sum(item_count) OVER (ORDER BY ordinal ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS expected_start
            FROM memory_read_chunks WHERE repo_id=NEW.repo_id AND read_id=NEW.read_id) indexed;
    IF invalid OR actual_chunks<>NEW.chunk_count OR actual_items<>NEW.item_count OR actual_bytes<>NEW.body_bytes THEN
        RAISE EXCEPTION 'memory read is incomplete' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_read_complete AFTER INSERT ON memory_read_headers
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_read_complete();
