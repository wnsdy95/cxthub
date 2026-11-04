CREATE FUNCTION public.cxt_account_legacy_checkpoint_insert() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE added record;
BEGIN
  IF TG_RELID<>'public.doc_legacy_byte_stage_checkpoint_objects_v1'::regclass
     OR TG_NAME<>'account_legacy_checkpoint_insert' OR TG_WHEN<>'AFTER'
     OR TG_LEVEL<>'STATEMENT' OR TG_NARGS<>0 OR TG_OP<>'INSERT' THEN
    RAISE EXCEPTION 'legacy checkpoint accounting requires its insert statement' USING ERRCODE='23514';
  END IF;
  FOR added IN
    SELECT doc_hash,count(*) AS objects,sum(octet_length(bytes)) AS bytes
    FROM cxt_inserted_legacy_checkpoint_objects GROUP BY doc_hash ORDER BY doc_hash
  LOOP
    UPDATE public.doc_legacy_byte_usage_v1
      SET checkpoint_objects=checkpoint_objects+added.objects,
          checkpoint_bytes=checkpoint_bytes+added.bytes
      WHERE doc_hash=added.doc_hash;
    IF NOT FOUND AND EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=added.doc_hash) THEN
      RAISE EXCEPTION 'legacy checkpoint storage requires its usage row' USING ERRCODE='23514';
    END IF;
  END LOOP;
  RETURN NULL;
END;
$$;

REVOKE EXECUTE ON FUNCTION public.cxt_account_legacy_checkpoint_insert() FROM PUBLIC;
DO $$
DECLARE trusted_owner name;
BEGIN
  SELECT pg_get_userbyid(relowner) INTO STRICT trusted_owner FROM pg_class WHERE oid='public.doc_legacy_byte_usage_v1'::regclass;
  EXECUTE format('ALTER FUNCTION public.cxt_account_legacy_checkpoint_insert() OWNER TO %I',trusted_owner);
END;
$$;

DROP TRIGGER account_legacy_checkpoint_objects ON public.doc_legacy_byte_stage_checkpoint_objects_v1;
CREATE TRIGGER account_legacy_checkpoint_insert AFTER INSERT ON public.doc_legacy_byte_stage_checkpoint_objects_v1
  REFERENCING NEW TABLE AS cxt_inserted_legacy_checkpoint_objects
  FOR EACH STATEMENT EXECUTE FUNCTION public.cxt_account_legacy_checkpoint_insert();
