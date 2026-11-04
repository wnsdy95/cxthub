ALTER TABLE doc_legacy_byte_stages_v1
  ADD COLUMN stage_id uuid NOT NULL DEFAULT gen_random_uuid() CHECK (stage_id<>'00000000-0000-0000-0000-000000000000'::uuid),
  ADD COLUMN header_version integer NOT NULL DEFAULT 0,
  ADD COLUMN header_bytes integer,
  ADD COLUMN header_next integer NOT NULL DEFAULT 0,
  ADD COLUMN header_complete boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT legacy_byte_stage_header_extent CHECK (
    (header_version=0 AND header_bytes IS NULL AND header_next=0 AND NOT header_complete)
    OR (header_version=1 AND header_bytes IS NOT NULL AND header_bytes BETWEEN 2 AND 536870912
      AND header_next BETWEEN 0 AND (header_bytes+65535)/65536
      AND header_complete=(header_next=(header_bytes+65535)/65536))
  );

CREATE TABLE doc_legacy_byte_stage_headers_v1 (
  doc_hash text NOT NULL REFERENCES doc_legacy_byte_stages_v1(doc_hash) ON DELETE CASCADE,
  ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 8191),
  hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 65536),
  PRIMARY KEY(doc_hash,ordinal),
  CHECK (hash='sha256:'||encode(sha256(bytes),'hex'))
);

CREATE FUNCTION cxt_guard_legacy_byte_header_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN
    NEW.stage_id := gen_random_uuid();
    IF NEW.header_version<>0 OR NEW.header_bytes IS NOT NULL OR NEW.header_next<>0 OR NEW.header_complete THEN
      RAISE EXCEPTION 'legacy byte header must start undeclared' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.stage_id IS DISTINCT FROM OLD.stage_id THEN
    RAISE EXCEPTION 'legacy byte stage incarnation is immutable' USING ERRCODE='23514';
  END IF;
  IF (NEW.header_version,NEW.header_bytes,NEW.header_next,NEW.header_complete)
      IS NOT DISTINCT FROM (OLD.header_version,OLD.header_bytes,OLD.header_next,OLD.header_complete) THEN
    RETURN NEW;
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1'
     OR COALESCE(current_setting('cxt.legacy_byte_header_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte header requires stage and header protocol 1' USING ERRCODE='0A000';
  END IF;
  IF OLD.header_version=0 THEN
    IF NEW.header_version<>1 OR NEW.header_next<>0 OR NEW.header_complete THEN
      RAISE EXCEPTION 'legacy byte header initialization must start at zero' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.header_version<>OLD.header_version OR NEW.header_bytes IS DISTINCT FROM OLD.header_bytes
     OR OLD.header_complete OR NEW.header_next<>OLD.header_next+1 THEN
    RAISE EXCEPTION 'legacy byte header declaration or checkpoint changed' USING ERRCODE='23514';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_headers_v1 part
    WHERE part.doc_hash=NEW.doc_hash AND part.ordinal=OLD.header_next
      AND octet_length(part.bytes)=LEAST(65536,NEW.header_bytes-OLD.header_next*65536)) THEN
    RAISE EXCEPTION 'legacy byte header checkpoint skipped its part' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_byte_header_stage BEFORE INSERT OR UPDATE ON doc_legacy_byte_stages_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_header_stage();

CREATE FUNCTION cxt_guard_legacy_byte_header_part() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage doc_legacy_byte_stages_v1%ROWTYPE;
        current_source_id uuid;
        current_source_version bigint;
        expected_bytes bigint;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash) THEN
      RAISE EXCEPTION 'live legacy byte header parts are retained' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP='UPDATE' THEN
    RAISE EXCEPTION 'legacy byte header parts are immutable' USING ERRCODE='23514';
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1'
     OR COALESCE(current_setting('cxt.legacy_byte_header_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte header requires stage and header protocol 1' USING ERRCODE='0A000';
  END IF;
  SELECT incarnation.incarnation,source.doc_range_source_version INTO current_source_id,current_source_version
    FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE source.hash=NEW.doc_hash FOR SHARE OF source,incarnation;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'legacy byte header requires current original source' USING ERRCODE='23514';
  END IF;
  SELECT * INTO stage FROM doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'legacy byte header requires its stage' USING ERRCODE='23514';
  END IF;
  IF stage.source_id IS DISTINCT FROM current_source_id OR stage.source_version IS DISTINCT FROM current_source_version THEN
    RAISE EXCEPTION 'legacy byte header requires current original source' USING ERRCODE='23514';
  END IF;
  expected_bytes := LEAST(65536,stage.header_bytes-NEW.ordinal::bigint*65536);
  IF stage.header_version<>1 OR stage.header_complete OR NEW.ordinal IS DISTINCT FROM stage.header_next
     OR NEW.ordinal NOT BETWEEN 0 AND 8191 OR NEW.ordinal>=(stage.header_bytes+65535)/65536
     OR octet_length(NEW.bytes) IS DISTINCT FROM expected_bytes
     OR NEW.hash IS DISTINCT FROM 'sha256:'||encode(sha256(NEW.bytes),'hex') THEN
    RAISE EXCEPTION 'legacy byte header part is outside its declared extent' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_byte_header_parts BEFORE INSERT OR UPDATE OR DELETE ON doc_legacy_byte_stage_headers_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_header_part();

CREATE FUNCTION cxt_guard_legacy_byte_header_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS(SELECT 1 FROM doc_legacy_byte_stage_headers_v1 part
      WHERE part.doc_hash=NEW.doc_hash AND part.ordinal=NEW.ordinal AND part.hash=NEW.hash)
     AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 stage
       JOIN blobs source ON source.hash=stage.doc_hash
       JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
       WHERE stage.doc_hash=NEW.doc_hash AND stage.header_version=1 AND stage.header_next>NEW.ordinal
         AND stage.source_id=incarnation.incarnation AND stage.source_version=source.doc_range_source_version
         AND octet_length(NEW.bytes)=LEAST(65536,stage.header_bytes-NEW.ordinal*65536)) THEN
    RAISE EXCEPTION 'legacy byte header part requires its atomic current checkpoint' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER legacy_byte_header_checkpoint AFTER INSERT ON doc_legacy_byte_stage_headers_v1
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_header_checkpoint();
