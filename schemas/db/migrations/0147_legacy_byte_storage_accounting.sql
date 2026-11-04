LOCK TABLE doc_legacy_byte_stages_v1,doc_legacy_byte_stage_objects_v1,doc_legacy_byte_stage_parts_v1 IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE doc_legacy_byte_usage_v1 (
  doc_hash text PRIMARY KEY REFERENCES doc_legacy_byte_stages_v1(doc_hash) ON DELETE CASCADE,
  object_bytes bigint NOT NULL DEFAULT 0 CHECK (object_bytes>=0),
  object_count bigint NOT NULL DEFAULT 0 CHECK (object_count>=0),
  part_count bigint NOT NULL DEFAULT 0 CHECK (part_count>=0)
);

INSERT INTO doc_legacy_byte_usage_v1(doc_hash,object_bytes,object_count,part_count)
SELECT stage.doc_hash,coalesce(objects.bytes,0),coalesce(objects.objects,0),coalesce(parts.parts,0)
FROM doc_legacy_byte_stages_v1 stage
LEFT JOIN (SELECT doc_hash,sum(octet_length(bytes)) AS bytes,count(*) AS objects
  FROM doc_legacy_byte_stage_objects_v1 GROUP BY doc_hash) objects ON objects.doc_hash=stage.doc_hash
LEFT JOIN (SELECT doc_hash,count(*) AS parts FROM doc_legacy_byte_stage_parts_v1 GROUP BY doc_hash) parts ON parts.doc_hash=stage.doc_hash;

CREATE FUNCTION cxt_guard_legacy_byte_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF pg_trigger_depth()<2 THEN
    RAISE EXCEPTION 'legacy byte usage is maintained by source-local storage triggers' USING ERRCODE='23514';
  END IF;
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash) THEN
      RAISE EXCEPTION 'live legacy byte usage is retained' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP='UPDATE' AND NEW.doc_hash<>OLD.doc_hash THEN
    RAISE EXCEPTION 'legacy byte usage identity is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_byte_usage BEFORE INSERT OR UPDATE OR DELETE ON doc_legacy_byte_usage_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_byte_usage();

CREATE FUNCTION cxt_account_legacy_byte_storage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source_hash text;
        change bigint;
BEGIN
  IF TG_TABLE_NAME='doc_legacy_byte_stages_v1' THEN
    INSERT INTO doc_legacy_byte_usage_v1(doc_hash) VALUES(NEW.doc_hash);
    RETURN NEW;
  END IF;
  IF TG_OP='INSERT' THEN
    source_hash := NEW.doc_hash;
    change := 1;
  ELSE
    source_hash := OLD.doc_hash;
    change := -1;
  END IF;
  IF TG_TABLE_NAME='doc_legacy_byte_stage_objects_v1' THEN
    UPDATE doc_legacy_byte_usage_v1 SET object_count=object_count+change,
      object_bytes=object_bytes+change*CASE WHEN TG_OP='INSERT' THEN octet_length(NEW.bytes) ELSE octet_length(OLD.bytes) END
      WHERE doc_hash=source_hash;
  ELSE
    UPDATE doc_legacy_byte_usage_v1 SET part_count=part_count+change WHERE doc_hash=source_hash;
  END IF;
  IF NOT FOUND AND EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=source_hash) THEN
    RAISE EXCEPTION 'legacy byte storage requires its source-local usage row' USING ERRCODE='23514';
  END IF;
  RETURN CASE WHEN TG_OP='INSERT' THEN NEW ELSE OLD END;
END;
$$;

CREATE TRIGGER account_legacy_byte_source AFTER INSERT ON doc_legacy_byte_stages_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_account_legacy_byte_storage();
CREATE TRIGGER account_legacy_byte_objects AFTER INSERT OR DELETE ON doc_legacy_byte_stage_objects_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_account_legacy_byte_storage();
CREATE TRIGGER account_legacy_byte_parts AFTER INSERT OR DELETE ON doc_legacy_byte_stage_parts_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_account_legacy_byte_storage();
