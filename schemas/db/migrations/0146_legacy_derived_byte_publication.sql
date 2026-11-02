ALTER TABLE doc_range_generations_v2 DROP CONSTRAINT doc_range_generations_v2_range_layout_version_check;
ALTER TABLE doc_range_generations_v2
  ADD CONSTRAINT doc_range_layout_version_known CHECK (range_layout_version IN (0,2,3,4,5));

ALTER TABLE doc_legacy_byte_stages_v1 ADD CONSTRAINT legacy_byte_stage_plan UNIQUE(doc_hash,plan_hash);
ALTER TABLE doc_range_generations_v2
  ADD COLUMN legacy_byte_plan_hash text,
  ADD CONSTRAINT derived_range_cache_link CHECK ((range_layout_version=5)=(legacy_byte_plan_hash IS NOT NULL)),
  ADD CONSTRAINT derived_range_cache_retention FOREIGN KEY(doc_hash,legacy_byte_plan_hash)
    REFERENCES doc_legacy_byte_stages_v1(doc_hash,plan_hash) ON DELETE CASCADE;

CREATE OR REPLACE FUNCTION cxt_doc_range_layout_hint() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
BEGIN
  NEW.range_layout_version := 0;
  NEW.range_stream_bytes := NULL;
  BEGIN
    payload := convert_from(NEW.descriptor,'UTF8')::jsonb;
    IF jsonb_typeof(payload->'version')='number'
       AND jsonb_typeof(payload->'stream_bytes')='number'
       AND (payload->>'version')::integer IN (2,3,4,5)
       AND (payload->>'stream_bytes')::bigint>=0 THEN
      NEW.range_layout_version := (payload->>'version')::smallint;
      NEW.range_stream_bytes := (payload->>'stream_bytes')::bigint;
    END IF;
  EXCEPTION WHEN OTHERS THEN
    NEW.range_layout_version := 0;
    NEW.range_stream_bytes := NULL;
  END;
  RETURN NEW;
END;
$$;

CREATE FUNCTION cxt_guard_legacy_derived_range() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.legacy_byte_plan_hash IS NULL THEN
    RETURN NEW;
  END IF;
  IF COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'')<>'5' THEN
    RAISE EXCEPTION 'derived byte publication requires worker protocol 5' USING ERRCODE='0A000';
  END IF;
  IF NEW.range_layout_version<>5 OR NOT EXISTS(
    SELECT 1 FROM doc_legacy_byte_stages_v1 stage
    JOIN blobs source ON source.hash=stage.doc_hash
    JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE stage.doc_hash=NEW.doc_hash AND stage.plan_hash=NEW.legacy_byte_plan_hash
      AND stage.complete AND stage.next_block=stage.blocks
      AND stage.source_id=incarnation.incarnation
      AND stage.source_version=source.doc_range_source_version AND stage.source_version=NEW.source_version
      AND (convert_from(stage.spec,'UTF8')::jsonb->>'stream_bytes')::bigint=NEW.range_stream_bytes
      AND convert_from(stage.spec,'UTF8')::jsonb#>>'{reference,Hash}'=NEW.doc_hash
      AND convert_from(stage.spec,'UTF8')::jsonb#>>'{reference,Identity}'=''
      AND (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 part WHERE part.doc_hash=stage.doc_hash)=stage.blocks
  ) THEN
    RAISE EXCEPTION 'derived range requires a complete current source cache' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_derived_range BEFORE INSERT OR UPDATE ON doc_range_generations_v2
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_derived_range();

CREATE OR REPLACE FUNCTION cxt_guard_doc_range_byte_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='UPDATE' AND NEW.minimum_range_version<OLD.minimum_range_version THEN
    RAISE EXCEPTION 'document range requirements cannot move backwards' USING ERRCODE='23514';
  END IF;
  IF NEW.minimum_range_version=4 THEN
    IF ((NEW.state='running' AND (TG_OP='INSERT' OR OLD.state<>'running' OR NEW.version<>OLD.version))
        OR (NEW.state='completed' AND (TG_OP='INSERT' OR OLD.state<>'completed' OR NEW.version<>OLD.version)))
       AND COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'') NOT IN ('4','5') THEN
      RAISE EXCEPTION 'bounded byte jobs require worker protocol 4 or 5' USING ERRCODE='0A000';
    END IF;
    IF NEW.state='completed' AND EXISTS(SELECT 1 FROM doc_range_generations_v2
      WHERE doc_hash=NEW.doc_hash AND legacy_byte_plan_hash IS NOT NULL)
      AND COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'')<>'5' THEN
      RAISE EXCEPTION 'derived byte completion requires worker protocol 5' USING ERRCODE='0A000';
    END IF;
    IF NEW.state='completed' AND NOT EXISTS (
      SELECT 1 FROM doc_range_generations_v2 generation
      JOIN blobs source ON source.hash=generation.doc_hash
      JOIN repo_blobs owned ON owned.hash=source.hash AND owned.kind='doc' AND owned.repo_id=NEW.repo_id
      WHERE generation.doc_hash=NEW.doc_hash AND generation.source_version=source.doc_range_source_version
        AND NEW.byte_source_version=source.doc_range_source_version
        AND (generation.range_layout_version>=4 OR generation.range_stream_bytes=0)
    ) THEN
      RAISE EXCEPTION 'bounded byte job completion requires current byte generation' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
