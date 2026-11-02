CREATE OR REPLACE FUNCTION public.cxt_guard_checkpoint_structure() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
        reference jsonb;
        source_bytes bytea;
        expected_bytes integer;
        child_bytes integer;
        height integer;
        stage public.doc_legacy_byte_stages_v1%ROWTYPE;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
      WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND hash=OLD.hash) THEN
      RAISE EXCEPTION 'checkpoint structures are immutable with their object' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP='UPDATE' THEN
    RAISE EXCEPTION 'checkpoint structures are immutable' USING ERRCODE='23514';
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'checkpoint structures require protocol 1' USING ERRCODE='0A000';
  END IF;
  SELECT * INTO STRICT stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id FOR UPDATE;
  IF stage.plan_hash IS NOT NULL THEN
    RAISE EXCEPTION 'checkpoint structures require a preparing stage' USING ERRCODE='23514';
  END IF;
  SELECT bytes INTO STRICT source_bytes FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
    WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id AND hash=NEW.hash;
  BEGIN
    payload := convert_from(source_bytes,'UTF8')::jsonb;
    IF payload->>'v' IS DISTINCT FROM '1' OR jsonb_typeof(payload) IS DISTINCT FROM 'object' THEN
      RAISE EXCEPTION 'unsupported checkpoint structure';
    END IF;
    IF NEW.kind=1 THEN
      IF jsonb_typeof(payload->'e') IS DISTINCT FROM 'array' OR jsonb_array_length(payload->'e') NOT BETWEEN 1 AND 32
         OR (SELECT count(*) FROM jsonb_object_keys(payload))<>2 THEN
        RAISE EXCEPTION 'invalid checkpoint index structure';
      END IF;
    ELSIF NEW.kind=2 THEN
      IF jsonb_typeof(payload->'s') IS DISTINCT FROM 'object' OR (
        payload->'s'->>'Version'='1' AND NOT (payload ? 'parser')
        OR payload->'s'->>'Version'='2' AND payload->'s'->>'Phase'='0'
          AND jsonb_typeof(payload->'parser')='object') IS DISTINCT FROM TRUE THEN
        RAISE EXCEPTION 'invalid checkpoint root structure';
      END IF;
      IF payload->'s'->>'Version'='2' AND NOT EXISTS(
        SELECT 1 FROM public.doc_legacy_checkpoint_structures_v1
        WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id AND hash=payload->'parser'->'pins'->>'h' AND kind=1) THEN
        RAISE EXCEPTION 'parser checkpoint requires its retained object index';
      END IF;
    END IF;
    FOR reference IN SELECT DISTINCT value FROM jsonb_path_query(payload,'strict $.**') value
      WHERE jsonb_typeof(value)='object' AND jsonb_typeof(value->'h')='string' AND value->>'h'<>''
    LOOP
      expected_bytes := (reference->>'n')::integer;
      height := COALESCE((reference->>'d')::integer,0);
      IF reference->>'h' !~ '^sha256:[0-9a-f]{64}$' OR reference->>'h'=NEW.hash OR expected_bytes NOT BETWEEN 1 AND 65536 OR height NOT BETWEEN 0 AND 3 THEN
        RAISE EXCEPTION 'invalid checkpoint child reference';
      END IF;
      SELECT octet_length(bytes) INTO child_bytes FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
        WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id AND hash=reference->>'h';
      IF NOT FOUND OR child_bytes IS DISTINCT FROM expected_bytes OR height>0 AND NOT EXISTS(
        SELECT 1 FROM public.doc_legacy_checkpoint_structures_v1
        WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id AND hash=reference->>'h' AND kind=1) THEN
        RAISE EXCEPTION 'checkpoint child is not a retained typed object';
      END IF;
    END LOOP;
  EXCEPTION WHEN OTHERS THEN
    RAISE EXCEPTION 'checkpoint structure is invalid: %',SQLERRM USING ERRCODE='23514';
  END;
  RETURN NEW;
END;
$$;
