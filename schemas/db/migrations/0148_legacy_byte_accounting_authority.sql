CREATE OR REPLACE FUNCTION cxt_guard_legacy_byte_usage() RETURNS trigger
LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE trusted_owner name;
BEGIN
  SELECT pg_get_userbyid(relowner) INTO STRICT trusted_owner FROM pg_class
    WHERE oid='public.doc_legacy_byte_usage_v1'::regclass;
  IF TG_RELID<>'public.doc_legacy_byte_usage_v1'::regclass
     OR current_user<>trusted_owner OR pg_trigger_depth()<2 THEN
    RAISE EXCEPTION 'legacy byte usage requires its trusted accounting owner' USING ERRCODE='23514';
  END IF;
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash) THEN
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

CREATE OR REPLACE FUNCTION cxt_account_legacy_byte_storage() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE source_hash text;
        change bigint;
BEGIN
  IF TG_WHEN<>'AFTER' OR TG_LEVEL<>'ROW' OR TG_NARGS<>0 THEN
    RAISE EXCEPTION 'legacy byte accounting requires its source storage event' USING ERRCODE='23514';
  END IF;
  IF TG_RELID='public.doc_legacy_byte_stages_v1'::regclass AND TG_OP='INSERT' THEN
    INSERT INTO public.doc_legacy_byte_usage_v1(doc_hash) VALUES(NEW.doc_hash);
    RETURN NEW;
  END IF;
  IF TG_RELID NOT IN ('public.doc_legacy_byte_stage_objects_v1'::regclass,'public.doc_legacy_byte_stage_parts_v1'::regclass)
     OR TG_OP NOT IN ('INSERT','DELETE') THEN
    RAISE EXCEPTION 'legacy byte accounting rejects unrelated storage events' USING ERRCODE='23514';
  END IF;
  IF TG_OP='INSERT' THEN
    source_hash := NEW.doc_hash;
    change := 1;
  ELSE
    source_hash := OLD.doc_hash;
    change := -1;
  END IF;
  IF TG_RELID='public.doc_legacy_byte_stage_objects_v1'::regclass THEN
    UPDATE public.doc_legacy_byte_usage_v1 SET object_count=object_count+change,
      object_bytes=object_bytes+change*CASE WHEN TG_OP='INSERT' THEN octet_length(NEW.bytes) ELSE octet_length(OLD.bytes) END
      WHERE doc_hash=source_hash;
  ELSE
    UPDATE public.doc_legacy_byte_usage_v1 SET part_count=part_count+change WHERE doc_hash=source_hash;
  END IF;
  IF NOT FOUND AND EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=source_hash) THEN
    RAISE EXCEPTION 'legacy byte storage requires its source-local usage row' USING ERRCODE='23514';
  END IF;
  RETURN CASE WHEN TG_OP='INSERT' THEN NEW ELSE OLD END;
END;
$$;

REVOKE ALL ON public.doc_legacy_byte_usage_v1 FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION public.cxt_account_legacy_byte_storage() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION public.cxt_guard_legacy_byte_usage() FROM PUBLIC;
