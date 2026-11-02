ALTER TABLE blobs ADD COLUMN doc_range_source_version bigint NOT NULL DEFAULT 0
  CHECK (doc_range_source_version >= 0);

CREATE FUNCTION cxt_advance_doc_range_source_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.doc_range_source_version := OLD.doc_range_source_version + 1;
  RETURN NEW;
END;
$$;

CREATE TRIGGER advance_doc_range_source_version
  BEFORE UPDATE ON blobs
  FOR EACH ROW EXECUTE FUNCTION cxt_advance_doc_range_source_version();

CREATE TABLE doc_range_generations_v2 (
  doc_hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
  source_version bigint NOT NULL CHECK (source_version >= 0),
  generation_hash text NOT NULL,
  descriptor bytea NOT NULL CHECK (octet_length(descriptor) BETWEEN 1 AND 264192)
);

CREATE TABLE doc_range_objects_v2 (
  doc_hash text NOT NULL REFERENCES doc_range_generations_v2(doc_hash) ON DELETE CASCADE,
  hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 131072),
  PRIMARY KEY (doc_hash, hash)
);

CREATE FUNCTION cxt_invalidate_doc_range_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM doc_range_generations_v2 WHERE doc_hash=OLD.hash;
  RETURN NEW;
END;
$$;

CREATE TRIGGER invalidate_doc_range_generation
  AFTER UPDATE OF bytes ON blobs
  FOR EACH ROW EXECUTE FUNCTION cxt_invalidate_doc_range_generation();
