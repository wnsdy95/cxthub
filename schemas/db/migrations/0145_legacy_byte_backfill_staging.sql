CREATE TABLE doc_legacy_byte_stages_v1 (
  doc_hash text PRIMARY KEY REFERENCES blob_source_incarnations(hash) ON DELETE CASCADE,
  source_id uuid NOT NULL CHECK (source_id<>'00000000-0000-0000-0000-000000000000'::uuid),
  source_version bigint NOT NULL CHECK (source_version>=0),
  plan_hash text NOT NULL CHECK (plan_hash ~ '^sha256:[0-9a-f]{64}$'),
  spec bytea NOT NULL CHECK (octet_length(spec) BETWEEN 1 AND 1024),
  blocks integer NOT NULL CHECK (blocks BETWEEN 0 AND 1024),
  next_block integer NOT NULL CHECK (next_block>=0 AND next_block<=blocks),
  complete boolean NOT NULL CHECK (complete=(next_block=blocks)),
  CHECK (plan_hash='sha256:'||encode(sha256(spec),'hex'))
);

CREATE TABLE doc_legacy_byte_stage_objects_v1 (
  doc_hash text NOT NULL REFERENCES doc_legacy_byte_stages_v1(doc_hash) ON DELETE CASCADE,
  hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 524288),
  PRIMARY KEY(doc_hash,hash),
  CHECK (hash='sha256:'||encode(sha256(bytes),'hex'))
);

CREATE TABLE doc_legacy_byte_stage_parts_v1 (
  doc_hash text NOT NULL REFERENCES doc_legacy_byte_stages_v1(doc_hash) ON DELETE CASCADE,
  ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 1023),
  hash text NOT NULL,
  byte_count integer NOT NULL CHECK (byte_count BETWEEN 1 AND 524288),
  PRIMARY KEY(doc_hash,ordinal),
  FOREIGN KEY(doc_hash,hash) REFERENCES doc_legacy_byte_stage_objects_v1(doc_hash,hash) ON DELETE CASCADE
);

CREATE FUNCTION cxt_guard_legacy_byte_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte staging requires protocol 1' USING ERRCODE='0A000';
  END IF;
  IF TG_OP='INSERT' AND NEW.next_block<>0 THEN
    RAISE EXCEPTION 'legacy byte staging must start at zero' USING ERRCODE='23514';
  END IF;
  IF TG_OP='UPDATE' AND (
    (NEW.doc_hash,NEW.source_id,NEW.source_version,NEW.plan_hash,NEW.spec,NEW.blocks)
      IS DISTINCT FROM (OLD.doc_hash,OLD.source_id,OLD.source_version,OLD.plan_hash,OLD.spec,OLD.blocks)
    OR NEW.next_block<OLD.next_block OR NEW.next_block>OLD.next_block+8
  ) THEN
    RAISE EXCEPTION 'legacy byte checkpoint or source changed' USING ERRCODE='23514';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE source.hash=NEW.doc_hash AND source.doc_range_source_version=NEW.source_version AND incarnation.incarnation=NEW.source_id) THEN
    RAISE EXCEPTION 'legacy byte staging requires current original source' USING ERRCODE='23514';
  END IF;
  IF TG_OP='UPDATE' AND NEW.next_block<>OLD.next_block AND
    (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 part
      WHERE part.doc_hash=NEW.doc_hash AND part.ordinal>=OLD.next_block AND part.ordinal<NEW.next_block)<>NEW.next_block-OLD.next_block THEN
    RAISE EXCEPTION 'legacy byte checkpoint skipped blocks' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_byte_stage BEFORE INSERT OR UPDATE ON doc_legacy_byte_stages_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_stage();

CREATE FUNCTION cxt_guard_legacy_byte_stage_object() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage doc_legacy_byte_stages_v1%ROWTYPE;
        expected_bytes bigint;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash) THEN
      RAISE EXCEPTION 'live legacy byte staging objects are retained' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP='UPDATE' THEN
    RAISE EXCEPTION 'legacy byte staging objects are immutable' USING ERRCODE='23514';
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte staging requires protocol 1' USING ERRCODE='0A000';
  END IF;
  IF TG_TABLE_NAME='doc_legacy_byte_stage_parts_v1' THEN
    SELECT * INTO STRICT stage FROM doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash;
    expected_bytes := LEAST(524288,(convert_from(stage.spec,'UTF8')::jsonb->>'stream_bytes')::bigint-NEW.ordinal::bigint*524288);
    IF stage.complete OR NEW.ordinal<stage.next_block OR NEW.ordinal>=stage.blocks
       OR NEW.ordinal>=stage.next_block+8 OR NEW.byte_count<>expected_bytes
       OR NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_objects_v1 object
          WHERE object.doc_hash=NEW.doc_hash AND object.hash=NEW.hash AND octet_length(object.bytes)=NEW.byte_count) THEN
      RAISE EXCEPTION 'legacy byte part is outside the verified batch extent' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_byte_stage_objects BEFORE INSERT OR UPDATE OR DELETE ON doc_legacy_byte_stage_objects_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_stage_object();
CREATE TRIGGER guard_legacy_byte_stage_parts BEFORE INSERT OR UPDATE OR DELETE ON doc_legacy_byte_stage_parts_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_stage_object();
