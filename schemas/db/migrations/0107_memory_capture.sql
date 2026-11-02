CREATE TABLE memory_capture_headers (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    capture_id text COLLATE "C" NOT NULL CHECK (capture_id ~ '^sha256:[0-9a-f]{64}$'),
    header bytea NOT NULL CHECK (octet_length(header) BETWEEN 1 AND 16384),
    part_count integer NOT NULL CHECK (part_count BETWEEN 1 AND 110000),
    body_bytes bigint NOT NULL CHECK (body_bytes BETWEEN 0 AND 117440512),
    key_bytes bigint NOT NULL CHECK (key_bytes BETWEEN 0 AND 117440512),
    complete boolean NOT NULL CHECK (complete),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(created_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    PRIMARY KEY (repo_id, capture_id),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours'),
    CHECK (body_bytes + key_bytes + octet_length(header) + part_count::bigint * 128 <= 117440512)
);

CREATE INDEX memory_capture_expiry ON memory_capture_headers(repo_id, expires_at, capture_id);

CREATE TABLE memory_capture_blocks (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    block_hash text COLLATE "C" NOT NULL CHECK (block_hash ~ '^sha256:[0-9a-f]{64}$'),
    body bytea NOT NULL CHECK (octet_length(body) <= 1048576),
    PRIMARY KEY (repo_id, block_hash)
);

CREATE TABLE memory_capture_parts (
    repo_id text NOT NULL,
    capture_id text COLLATE "C" NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 109999),
    kind text COLLATE "C" NOT NULL CHECK (octet_length(kind) BETWEEN 1 AND 64),
    entity_key text COLLATE "C" NOT NULL CHECK (octet_length(entity_key) BETWEEN 1 AND 4096),
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    block_hash text COLLATE "C" NOT NULL,
    PRIMARY KEY (repo_id, capture_id, ordinal),
    UNIQUE (repo_id, capture_id, kind, entity_key),
    FOREIGN KEY (repo_id, capture_id) REFERENCES memory_capture_headers(repo_id, capture_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (repo_id, block_hash) REFERENCES memory_capture_blocks(repo_id, block_hash)
);

CREATE INDEX memory_capture_block_references ON memory_capture_parts(repo_id, block_hash);

CREATE FUNCTION cxt_memory_capture_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'memory capture data is immutable' USING ERRCODE='23514';
END
$$;

CREATE TRIGGER memory_capture_header_immutable BEFORE UPDATE ON memory_capture_headers
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();
CREATE TRIGGER memory_capture_block_immutable BEFORE UPDATE ON memory_capture_blocks
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();
CREATE TRIGGER memory_capture_part_immutable BEFORE UPDATE ON memory_capture_parts
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_immutable();
