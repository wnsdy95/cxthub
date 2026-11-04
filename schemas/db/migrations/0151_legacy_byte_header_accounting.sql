LOCK TABLE public.doc_legacy_byte_stages_v1,public.doc_legacy_byte_stage_objects_v1,
  public.doc_legacy_byte_stage_parts_v1,public.doc_legacy_byte_stage_headers_v1,
  public.doc_legacy_byte_usage_v1 IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE public.doc_legacy_byte_usage_v1
  ADD COLUMN header_bytes bigint NOT NULL DEFAULT 0 CHECK (header_bytes>=0),
  ADD COLUMN header_parts bigint NOT NULL DEFAULT 0 CHECK (header_parts>=0);

CREATE OR REPLACE FUNCTION public.cxt_account_legacy_byte_storage() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE source_hash text;
        change bigint;
BEGIN
  IF TG_WHEN<>'AFTER' OR TG_LEVEL<>'ROW' OR TG_NARGS<>0 THEN
    RAISE EXCEPTION 'legacy byte accounting requires its source storage event' USING ERRCODE='23514';
  END IF;
  IF TG_RELID='public.doc_legacy_byte_stages_v1'::regclass AND TG_NAME='account_legacy_byte_source' AND TG_OP='INSERT' THEN
    INSERT INTO public.doc_legacy_byte_usage_v1(doc_hash) VALUES(NEW.doc_hash);
    RETURN NEW;
  END IF;
  IF NOT (
    (TG_RELID='public.doc_legacy_byte_stage_objects_v1'::regclass AND TG_NAME='account_legacy_byte_objects')
    OR (TG_RELID='public.doc_legacy_byte_stage_parts_v1'::regclass AND TG_NAME='account_legacy_byte_parts')
    OR (TG_RELID='public.doc_legacy_byte_stage_headers_v1'::regclass AND TG_NAME='account_legacy_byte_headers')
  ) OR TG_OP NOT IN ('INSERT','DELETE') THEN
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
  ELSIF TG_RELID='public.doc_legacy_byte_stage_parts_v1'::regclass THEN
    UPDATE public.doc_legacy_byte_usage_v1 SET part_count=part_count+change WHERE doc_hash=source_hash;
  ELSE
    UPDATE public.doc_legacy_byte_usage_v1 SET header_parts=header_parts+change,
      header_bytes=header_bytes+change*CASE WHEN TG_OP='INSERT' THEN octet_length(NEW.bytes) ELSE octet_length(OLD.bytes) END
      WHERE doc_hash=source_hash;
  END IF;
  IF NOT FOUND AND EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=source_hash) THEN
    RAISE EXCEPTION 'legacy byte storage requires its source-local usage row' USING ERRCODE='23514';
  END IF;
  RETURN CASE WHEN TG_OP='INSERT' THEN NEW ELSE OLD END;
END;
$$;

REVOKE EXECUTE ON FUNCTION public.cxt_account_legacy_byte_storage() FROM PUBLIC;

CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage();

CREATE FUNCTION public.cxt_backfill_legacy_byte_header_usage() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
  IF TG_RELID<>'public.doc_legacy_byte_stage_headers_v1'::regclass
     OR TG_NAME<>'backfill_legacy_byte_header_usage' OR TG_WHEN<>'AFTER'
     OR TG_LEVEL<>'STATEMENT' OR TG_OP<>'INSERT' OR TG_NARGS<>0 THEN
    RAISE EXCEPTION 'legacy byte header bootstrap requires its source storage event' USING ERRCODE='23514';
  END IF;
  UPDATE public.doc_legacy_byte_usage_v1 usage
    SET header_bytes=headers.bytes,header_parts=headers.parts
    FROM (SELECT doc_hash,sum(octet_length(bytes))::bigint AS bytes,count(*) AS parts
      FROM public.doc_legacy_byte_stage_headers_v1 GROUP BY doc_hash) headers
    WHERE usage.doc_hash=headers.doc_hash;
  RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION public.cxt_backfill_legacy_byte_header_usage() FROM PUBLIC;

DO $$
DECLARE trusted_owner name;
BEGIN
  SELECT pg_get_userbyid(relowner) INTO STRICT trusted_owner FROM pg_class
    WHERE oid='public.doc_legacy_byte_usage_v1'::regclass;
  EXECUTE format('ALTER FUNCTION public.cxt_backfill_legacy_byte_header_usage() OWNER TO %I',trusted_owner);
END;
$$;

CREATE TRIGGER backfill_legacy_byte_header_usage AFTER INSERT ON public.doc_legacy_byte_stage_headers_v1
  FOR EACH STATEMENT EXECUTE FUNCTION public.cxt_backfill_legacy_byte_header_usage();

INSERT INTO public.doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes)
  SELECT NULL::text,NULL::integer,NULL::text,NULL::bytea WHERE false;

DROP TRIGGER backfill_legacy_byte_header_usage ON public.doc_legacy_byte_stage_headers_v1;
DROP FUNCTION public.cxt_backfill_legacy_byte_header_usage();
