ALTER TABLE public.doc_range_generations_v2
  DROP CONSTRAINT doc_range_layout_version_known,
  DROP CONSTRAINT derived_range_cache_link,
  ADD CONSTRAINT doc_range_layout_version_known CHECK (range_layout_version IN (0,2,3,4,5,6)),
  ADD CONSTRAINT derived_range_cache_link CHECK ((range_layout_version IN (5,6))=(legacy_byte_plan_hash IS NOT NULL));

CREATE OR REPLACE FUNCTION public.cxt_doc_range_layout_hint() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
BEGIN
  NEW.range_layout_version := 0;
  NEW.range_stream_bytes := NULL;
  BEGIN
    payload := convert_from(NEW.descriptor,'UTF8')::jsonb;
    IF jsonb_typeof(payload->'version')='number'
       AND jsonb_typeof(payload->'stream_bytes')='number'
       AND (payload->>'version')::integer IN (2,3,4,5,6)
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

CREATE OR REPLACE FUNCTION public.cxt_guard_legacy_derived_range() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
        reference jsonb;
        envelope_bytes bigint;
BEGIN
  IF NEW.legacy_byte_plan_hash IS NULL THEN
    RETURN NEW;
  END IF;
  IF COALESCE(current_setting('cxt.doc_range_worker_protocol',true),'')<>'5' THEN
    RAISE EXCEPTION 'derived byte publication requires worker protocol 5' USING ERRCODE='0A000';
  END IF;
  IF NEW.range_layout_version NOT IN (5,6) OR NOT EXISTS(
    SELECT 1 FROM public.doc_legacy_byte_stages_v1 stage
    JOIN public.blobs source ON source.hash=stage.doc_hash
    JOIN public.blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE stage.doc_hash=NEW.doc_hash AND stage.plan_hash=NEW.legacy_byte_plan_hash
      AND stage.complete AND stage.next_block=stage.blocks
      AND stage.source_id=incarnation.incarnation
      AND stage.source_version=source.doc_range_source_version AND stage.source_version=NEW.source_version
      AND (convert_from(stage.spec,'UTF8')::jsonb->>'stream_bytes')::bigint=NEW.range_stream_bytes
      AND convert_from(stage.spec,'UTF8')::jsonb#>>'{reference,Hash}'=NEW.doc_hash
      AND convert_from(stage.spec,'UTF8')::jsonb#>>'{reference,Identity}'=''
      AND (SELECT count(*) FROM public.doc_legacy_byte_stage_parts_v1 part WHERE part.doc_hash=stage.doc_hash)=stage.blocks
  ) THEN
    RAISE EXCEPTION 'derived range requires a complete current source cache' USING ERRCODE='23514';
  END IF;
  IF NEW.range_layout_version=6 THEN
    BEGIN
      payload := convert_from(NEW.descriptor,'UTF8')::jsonb;
      reference := payload->'envelope_ref';
      IF (payload ? 'envelope') IS DISTINCT FROM FALSE
         OR jsonb_typeof(reference) IS DISTINCT FROM 'object'
         OR jsonb_typeof(reference->'hash') IS DISTINCT FROM 'string'
         OR (reference->>'hash' ~ '^sha256:[0-9a-f]{64}$') IS DISTINCT FROM TRUE
         OR jsonb_typeof(reference->'bytes') IS DISTINCT FROM 'number'
         OR (reference->>'bytes' ~ '^[0-9]+$') IS DISTINCT FROM TRUE
         OR payload->>'source_identity' IS DISTINCT FROM ''
         OR payload->>'hash' IS DISTINCT FROM NEW.doc_hash THEN
        RAISE EXCEPTION 'invalid derived envelope reference';
      END IF;
      envelope_bytes := (reference->>'bytes')::bigint;
      IF envelope_bytes NOT BETWEEN 262145 AND 536870912
         OR NEW.range_stream_bytes>536870912-octet_length('{"envelope":,"events":[]}')-envelope_bytes
         OR NOT EXISTS(
           SELECT 1 FROM public.doc_legacy_byte_stages_v1 stage
           WHERE stage.doc_hash=NEW.doc_hash AND stage.plan_hash=NEW.legacy_byte_plan_hash
             AND stage.source_version=NEW.source_version
             AND stage.header_version=1 AND stage.header_complete AND stage.header_bytes=envelope_bytes
             AND stage.header_next=(stage.header_bytes+65535)/65536
             AND (SELECT count(*) FROM public.doc_legacy_byte_stage_headers_v1 part WHERE part.doc_hash=stage.doc_hash)=stage.header_next
         ) THEN
        RAISE EXCEPTION 'derived envelope reference requires its complete header stage';
      END IF;
    EXCEPTION WHEN OTHERS THEN
      RAISE EXCEPTION 'derived envelope reference is invalid: %',SQLERRM USING ERRCODE='23514';
    END;
  END IF;
  RETURN NEW;
END;
$$;
