ALTER TABLE doc_range_generations_v2
  ADD COLUMN range_layout_version smallint NOT NULL DEFAULT 0 CHECK (range_layout_version IN (0,2,3,4)),
  ADD COLUMN range_stream_bytes bigint CHECK (range_stream_bytes >= 0);

CREATE FUNCTION cxt_doc_range_layout_hint() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
BEGIN
  NEW.range_layout_version := 0;
  NEW.range_stream_bytes := NULL;
  BEGIN
    payload := convert_from(NEW.descriptor,'UTF8')::jsonb;
    IF jsonb_typeof(payload->'version')='number'
       AND jsonb_typeof(payload->'stream_bytes')='number'
       AND (payload->>'version')::integer IN (2,3,4)
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

CREATE TRIGGER doc_range_layout_hint
  BEFORE INSERT OR UPDATE OF descriptor,generation_hash,source_version,range_layout_version,range_stream_bytes
  ON doc_range_generations_v2 FOR EACH ROW EXECUTE FUNCTION cxt_doc_range_layout_hint();

ALTER TABLE doc_range_jobs_v1
  ADD COLUMN minimum_range_version smallint NOT NULL DEFAULT 0 CHECK (minimum_range_version IN (0,4)),
  ADD COLUMN byte_source_version bigint,
  ADD CONSTRAINT doc_range_byte_requirement CHECK (
    (minimum_range_version=0 AND byte_source_version IS NULL)
    OR (minimum_range_version=4 AND base_hash='' AND byte_source_version>=0 AND byte_source_version IS NOT NULL)
  );

CREATE OR REPLACE FUNCTION cxt_guard_doc_range_worker_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.state='running' AND (OLD.state<>'running' OR NEW.version<>OLD.version)
     AND COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'') NOT IN ('3','4')
     AND EXISTS (
       SELECT 1 FROM snapshots
       WHERE repo_id=NEW.repo_id AND id IN (NEW.doc_hash,NEW.base_hash)
         AND doc_hash=id AND doc_identity=''
     ) THEN
    RAISE EXCEPTION 'legacy document range jobs require worker protocol 3 or 4' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE FUNCTION cxt_guard_doc_range_byte_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='UPDATE' AND NEW.minimum_range_version<OLD.minimum_range_version THEN
    RAISE EXCEPTION 'document range requirements cannot move backwards' USING ERRCODE='23514';
  END IF;
  IF NEW.minimum_range_version=4 THEN
    IF ((NEW.state='running' AND (TG_OP='INSERT' OR OLD.state<>'running' OR NEW.version<>OLD.version))
        OR (NEW.state='completed' AND (TG_OP='INSERT' OR OLD.state<>'completed' OR NEW.version<>OLD.version)))
       AND COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'')<>'4' THEN
      RAISE EXCEPTION 'bounded byte jobs require worker protocol 4' USING ERRCODE='0A000';
    END IF;
    IF NEW.state='completed' AND NOT EXISTS (
      SELECT 1 FROM doc_range_generations_v2 generation
      JOIN blobs source ON source.hash=generation.doc_hash
      JOIN repo_blobs owned ON owned.hash=source.hash AND owned.kind='doc' AND owned.repo_id=NEW.repo_id
      WHERE generation.doc_hash=NEW.doc_hash
        AND generation.source_version=source.doc_range_source_version
        AND NEW.byte_source_version=source.doc_range_source_version
        AND (generation.range_layout_version>=4 OR generation.range_stream_bytes=0)
    ) THEN
      RAISE EXCEPTION 'bounded byte job completion requires current byte generation' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_range_byte_protocol
  BEFORE INSERT OR UPDATE OF state,version,minimum_range_version,byte_source_version
  ON doc_range_jobs_v1 FOR EACH ROW EXECUTE FUNCTION cxt_guard_doc_range_byte_protocol();
