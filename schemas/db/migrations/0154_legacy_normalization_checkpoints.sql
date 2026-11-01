LOCK TABLE public.doc_legacy_byte_stages_v1,public.doc_legacy_byte_usage_v1 IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE public.doc_legacy_byte_stages_v1
  ALTER COLUMN plan_hash DROP NOT NULL,
  ALTER COLUMN spec DROP NOT NULL,
  ALTER COLUMN blocks DROP NOT NULL,
  ADD COLUMN checkpoint_sequence bigint NOT NULL DEFAULT 0 CHECK (checkpoint_sequence>=0),
  ADD COLUMN checkpoint_hash text,
  ADD COLUMN checkpoint_bytes integer NOT NULL DEFAULT 0,
  ADD CONSTRAINT legacy_byte_preparing_extent CHECK (
    (plan_hash IS NULL AND spec IS NULL AND blocks IS NULL AND next_block=0 AND NOT complete
      AND header_version=0 AND header_bytes IS NULL AND header_next=0 AND NOT header_complete)
    OR (plan_hash IS NOT NULL AND spec IS NOT NULL AND blocks IS NOT NULL)
  ),
  ADD CONSTRAINT legacy_byte_checkpoint_extent CHECK (
    (checkpoint_hash IS NULL AND checkpoint_bytes=0)
    OR (checkpoint_hash IS NOT NULL AND checkpoint_hash ~ '^sha256:[0-9a-f]{64}$' AND checkpoint_sequence>0 AND checkpoint_bytes BETWEEN 1 AND 65536)
  ),
  ADD CONSTRAINT legacy_byte_stage_incarnation UNIQUE(doc_hash,stage_id);

ALTER TABLE public.doc_legacy_byte_usage_v1
  ADD COLUMN checkpoint_bytes bigint NOT NULL DEFAULT 0 CHECK (checkpoint_bytes BETWEEN 0 AND 2684420096),
  ADD COLUMN checkpoint_objects bigint NOT NULL DEFAULT 0 CHECK (checkpoint_objects BETWEEN 0 AND 65536);

CREATE TABLE public.doc_legacy_byte_stage_checkpoint_objects_v1 (
  doc_hash text NOT NULL,
  stage_id uuid NOT NULL,
  hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 65536),
  PRIMARY KEY(doc_hash,stage_id,hash),
  FOREIGN KEY(doc_hash,stage_id) REFERENCES public.doc_legacy_byte_stages_v1(doc_hash,stage_id) ON DELETE CASCADE,
  CHECK (hash='sha256:'||encode(sha256(bytes),'hex'))
);

CREATE OR REPLACE FUNCTION public.cxt_guard_legacy_byte_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte staging requires protocol 1' USING ERRCODE='0A000';
  END IF;
  IF TG_OP='INSERT' THEN
    IF NEW.next_block<>0 OR NEW.checkpoint_sequence<>0 OR NEW.checkpoint_hash IS NOT NULL OR NEW.checkpoint_bytes<>0 THEN
      RAISE EXCEPTION 'legacy byte staging must start at zero' USING ERRCODE='23514';
    END IF;
    IF NEW.plan_hash IS NULL AND COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
      RAISE EXCEPTION 'legacy normalization staging requires checkpoint protocol 1' USING ERRCODE='0A000';
    END IF;
  ELSE
    IF (NEW.doc_hash,NEW.source_id,NEW.source_version) IS DISTINCT FROM (OLD.doc_hash,OLD.source_id,OLD.source_version) THEN
      RAISE EXCEPTION 'legacy byte original source is immutable' USING ERRCODE='23514';
    END IF;
    IF (NEW.plan_hash,NEW.spec,NEW.blocks) IS DISTINCT FROM (OLD.plan_hash,OLD.spec,OLD.blocks) THEN
      IF OLD.plan_hash IS NOT NULL OR NEW.plan_hash IS NULL OR NEW.next_block<>0
         OR NEW.complete IS DISTINCT FROM (NEW.blocks=0)
         OR COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
        RAISE EXCEPTION 'legacy byte plan may be declared exactly once' USING ERRCODE='23514';
      END IF;
    ELSIF NEW.next_block<OLD.next_block OR NEW.next_block>OLD.next_block+8 THEN
      RAISE EXCEPTION 'legacy byte checkpoint changed' USING ERRCODE='23514';
    END IF;
    IF (NEW.checkpoint_sequence,NEW.checkpoint_hash,NEW.checkpoint_bytes)
        IS DISTINCT FROM (OLD.checkpoint_sequence,OLD.checkpoint_hash,OLD.checkpoint_bytes) THEN
      IF COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1'
         OR NEW.checkpoint_sequence<>OLD.checkpoint_sequence+1
         OR (NEW.checkpoint_hash IS NOT NULL AND NEW.plan_hash IS NOT NULL) THEN
        RAISE EXCEPTION 'legacy normalization checkpoint requires its next sequence' USING ERRCODE='23514';
      END IF;
    END IF;
    IF NEW.next_block<>OLD.next_block AND
      (SELECT count(*) FROM public.doc_legacy_byte_stage_parts_v1 part
        WHERE part.doc_hash=NEW.doc_hash AND part.ordinal>=OLD.next_block AND part.ordinal<NEW.next_block)<>NEW.next_block-OLD.next_block THEN
      RAISE EXCEPTION 'legacy byte checkpoint skipped blocks' USING ERRCODE='23514';
    END IF;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.blobs source JOIN public.blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE source.hash=NEW.doc_hash AND source.doc_range_source_version=NEW.source_version AND incarnation.incarnation=NEW.source_id) THEN
    RAISE EXCEPTION 'legacy byte staging requires current original source' USING ERRCODE='23514';
  END IF;
  IF NEW.checkpoint_hash IS NOT NULL AND NOT EXISTS(
    SELECT 1 FROM public.doc_legacy_byte_stage_checkpoint_objects_v1 object
      WHERE object.doc_hash=NEW.doc_hash AND object.stage_id=NEW.stage_id AND object.hash=NEW.checkpoint_hash
        AND octet_length(object.bytes)=NEW.checkpoint_bytes) THEN
    RAISE EXCEPTION 'legacy normalization checkpoint root is not pinned' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.cxt_guard_legacy_byte_stage_object() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage public.doc_legacy_byte_stages_v1%ROWTYPE;
        expected_bytes bigint;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash) THEN
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
  SELECT * INTO STRICT stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash;
  IF stage.plan_hash IS NULL THEN
    RAISE EXCEPTION 'canonical objects require a declared legacy byte plan' USING ERRCODE='23514';
  END IF;
  IF TG_TABLE_NAME='doc_legacy_byte_stage_parts_v1' THEN
    expected_bytes := LEAST(524288,(convert_from(stage.spec,'UTF8')::jsonb->>'stream_bytes')::bigint-NEW.ordinal::bigint*524288);
    IF stage.complete OR NEW.ordinal<stage.next_block OR NEW.ordinal>=stage.blocks
       OR NEW.ordinal>=stage.next_block+8 OR NEW.byte_count<>expected_bytes
       OR NOT EXISTS(SELECT 1 FROM public.doc_legacy_byte_stage_objects_v1 object
          WHERE object.doc_hash=NEW.doc_hash AND object.hash=NEW.hash AND octet_length(object.bytes)=NEW.byte_count) THEN
      RAISE EXCEPTION 'legacy byte part is outside the verified batch extent' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE FUNCTION public.cxt_guard_legacy_checkpoint_object() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage public.doc_legacy_byte_stages_v1%ROWTYPE;
        source_id uuid;
        source_version bigint;
BEGIN
  IF TG_OP='UPDATE' THEN
    RAISE EXCEPTION 'legacy checkpoint objects are immutable' USING ERRCODE='23514';
  END IF;
  IF TG_OP='DELETE' THEN
    SELECT * INTO stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash;
    IF FOUND AND (stage.checkpoint_hash IS NOT NULL OR COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1') THEN
      RAISE EXCEPTION 'live legacy checkpoint objects are pinned' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1'
     OR COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy checkpoint objects require protocol 1' USING ERRCODE='0A000';
  END IF;
  SELECT incarnation.incarnation,source.doc_range_source_version INTO source_id,source_version
    FROM public.blobs source JOIN public.blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE source.hash=NEW.doc_hash FOR SHARE OF source,incarnation;
  SELECT * INTO stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash FOR UPDATE;
  IF NOT FOUND OR stage.stage_id IS DISTINCT FROM NEW.stage_id OR stage.plan_hash IS NOT NULL
     OR stage.source_id IS DISTINCT FROM source_id OR stage.source_version IS DISTINCT FROM source_version THEN
    RAISE EXCEPTION 'legacy checkpoint object requires its current preparing stage' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_checkpoint_objects BEFORE INSERT OR UPDATE OR DELETE ON public.doc_legacy_byte_stage_checkpoint_objects_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_legacy_checkpoint_object();

CREATE FUNCTION public.cxt_account_legacy_checkpoint_storage() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE source_hash text;
        change bigint;
BEGIN
  IF TG_RELID<>'public.doc_legacy_byte_stage_checkpoint_objects_v1'::regclass OR TG_NAME<>'account_legacy_checkpoint_objects'
     OR TG_WHEN<>'AFTER' OR TG_LEVEL<>'ROW' OR TG_NARGS<>0 OR TG_OP NOT IN ('INSERT','DELETE') THEN
    RAISE EXCEPTION 'legacy checkpoint accounting requires its storage event' USING ERRCODE='23514';
  END IF;
  source_hash := CASE WHEN TG_OP='INSERT' THEN NEW.doc_hash ELSE OLD.doc_hash END;
  change := CASE WHEN TG_OP='INSERT' THEN 1 ELSE -1 END;
  UPDATE public.doc_legacy_byte_usage_v1 SET checkpoint_objects=checkpoint_objects+change,
    checkpoint_bytes=checkpoint_bytes+change*CASE WHEN TG_OP='INSERT' THEN octet_length(NEW.bytes) ELSE octet_length(OLD.bytes) END
    WHERE doc_hash=source_hash;
  IF NOT FOUND AND EXISTS(SELECT 1 FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=source_hash) THEN
    RAISE EXCEPTION 'legacy checkpoint storage requires its usage row' USING ERRCODE='23514';
  END IF;
  RETURN CASE WHEN TG_OP='INSERT' THEN NEW ELSE OLD END;
END;
$$;

REVOKE EXECUTE ON FUNCTION public.cxt_account_legacy_checkpoint_storage() FROM PUBLIC;
DO $$
DECLARE trusted_owner name;
BEGIN
  SELECT pg_get_userbyid(relowner) INTO STRICT trusted_owner FROM pg_class WHERE oid='public.doc_legacy_byte_usage_v1'::regclass;
  EXECUTE format('ALTER FUNCTION public.cxt_account_legacy_checkpoint_storage() OWNER TO %I',trusted_owner);
END;
$$;

CREATE TRIGGER account_legacy_checkpoint_objects AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_checkpoint_objects_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_checkpoint_storage();
