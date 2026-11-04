LOCK TABLE public.doc_legacy_byte_stages_v1,public.doc_legacy_byte_stage_checkpoint_objects_v1 IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE public.doc_legacy_byte_stages_v1
  ADD COLUMN checkpoint_gc_sequence bigint NOT NULL DEFAULT 0 CHECK (checkpoint_gc_sequence>=0),
  ADD COLUMN checkpoint_gc_last_sequence bigint NOT NULL DEFAULT -1 CHECK (checkpoint_gc_last_sequence>=-1),
  ADD COLUMN checkpoint_gc_last_hash text NOT NULL DEFAULT '' CHECK (checkpoint_gc_last_hash='' OR checkpoint_gc_last_hash ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN checkpoint_gc_changed boolean NOT NULL DEFAULT false,
  ADD COLUMN checkpoint_gc_complete boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT legacy_checkpoint_current_root FOREIGN KEY(doc_hash,stage_id,checkpoint_hash)
    REFERENCES public.doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE public.doc_legacy_checkpoint_staging_v1 (
  doc_hash text NOT NULL,
  stage_id uuid NOT NULL,
  hash text NOT NULL,
  last_sequence bigint NOT NULL CHECK (last_sequence>=0),
  PRIMARY KEY(doc_hash,stage_id,hash),
  FOREIGN KEY(doc_hash,stage_id,hash) REFERENCES public.doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash) ON DELETE CASCADE
);
CREATE INDEX legacy_checkpoint_staging_scan ON public.doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,last_sequence,hash);

INSERT INTO public.doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,hash,last_sequence)
 SELECT object.doc_hash,object.stage_id,object.hash,stage.checkpoint_sequence
 FROM public.doc_legacy_byte_stage_checkpoint_objects_v1 object
 JOIN public.doc_legacy_byte_stages_v1 stage ON stage.doc_hash=object.doc_hash AND stage.stage_id=object.stage_id;

CREATE TABLE public.doc_legacy_checkpoint_structures_v1 (
  doc_hash text NOT NULL,
  stage_id uuid NOT NULL,
  hash text NOT NULL,
  kind smallint NOT NULL CHECK (kind IN (1,2)),
  PRIMARY KEY(doc_hash,stage_id,hash),
  FOREIGN KEY(doc_hash,stage_id,hash) REFERENCES public.doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash) ON DELETE CASCADE
);

CREATE TABLE public.doc_legacy_checkpoint_edges_v1 (
  doc_hash text NOT NULL,
  stage_id uuid NOT NULL,
  parent_hash text NOT NULL,
  child_hash text NOT NULL,
  PRIMARY KEY(doc_hash,stage_id,parent_hash,child_hash),
  CHECK (parent_hash<>child_hash),
  FOREIGN KEY(doc_hash,stage_id,parent_hash) REFERENCES public.doc_legacy_checkpoint_structures_v1(doc_hash,stage_id,hash) ON DELETE CASCADE,
  FOREIGN KEY(doc_hash,stage_id,child_hash) REFERENCES public.doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX legacy_checkpoint_incoming ON public.doc_legacy_checkpoint_edges_v1(doc_hash,stage_id,child_hash);

CREATE FUNCTION public.cxt_guard_checkpoint_staging() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage public.doc_legacy_byte_stages_v1%ROWTYPE;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
      WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND hash=OLD.hash) THEN
      RAISE EXCEPTION 'checkpoint staging is retained with its object' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'checkpoint staging requires protocol 1' USING ERRCODE='0A000';
  END IF;
  SELECT * INTO STRICT stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id FOR UPDATE;
  IF NEW.last_sequence<>stage.checkpoint_sequence OR stage.plan_hash IS NOT NULL THEN
    RAISE EXCEPTION 'checkpoint staging requires its current preparing sequence' USING ERRCODE='23514';
  END IF;
  IF TG_OP='UPDATE' AND ((NEW.doc_hash,NEW.stage_id,NEW.hash) IS DISTINCT FROM (OLD.doc_hash,OLD.stage_id,OLD.hash) OR NEW.last_sequence<OLD.last_sequence) THEN
    RAISE EXCEPTION 'checkpoint staging identity and sequence are monotonic' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER guard_checkpoint_staging BEFORE INSERT OR UPDATE OR DELETE ON public.doc_legacy_checkpoint_staging_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_checkpoint_staging();

CREATE FUNCTION public.cxt_guard_checkpoint_structure() RETURNS trigger LANGUAGE plpgsql AS $$
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
      IF jsonb_typeof(payload->'s') IS DISTINCT FROM 'object' OR payload->'s'->>'Version' IS DISTINCT FROM '1' THEN
        RAISE EXCEPTION 'invalid checkpoint root structure';
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
CREATE TRIGGER guard_checkpoint_structures BEFORE INSERT OR UPDATE OR DELETE ON public.doc_legacy_checkpoint_structures_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_checkpoint_structure();

CREATE FUNCTION public.cxt_checkpoint_structure_edges() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE payload jsonb;
BEGIN
  SELECT convert_from(bytes,'UTF8')::jsonb INTO STRICT payload FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
    WHERE doc_hash=NEW.doc_hash AND stage_id=NEW.stage_id AND hash=NEW.hash;
  INSERT INTO public.doc_legacy_checkpoint_edges_v1(doc_hash,stage_id,parent_hash,child_hash)
    SELECT DISTINCT NEW.doc_hash,NEW.stage_id,NEW.hash,value->>'h' FROM jsonb_path_query(payload,'strict $.**') value
    WHERE jsonb_typeof(value)='object' AND jsonb_typeof(value->'h')='string' AND value->>'h'<>'';
  RETURN NEW;
END;
$$;
CREATE TRIGGER checkpoint_structure_edges AFTER INSERT ON public.doc_legacy_checkpoint_structures_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_checkpoint_structure_edges();

CREATE FUNCTION public.cxt_guard_checkpoint_edge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM public.doc_legacy_byte_stage_checkpoint_objects_v1
      WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND hash=OLD.parent_hash) THEN
      RAISE EXCEPTION 'checkpoint edges are retained with their parent' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
  END IF;
  IF TG_OP='UPDATE' OR pg_trigger_depth()<2 THEN
    RAISE EXCEPTION 'checkpoint edges are derived from immutable structure bytes' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER guard_checkpoint_edges BEFORE INSERT OR UPDATE OR DELETE ON public.doc_legacy_checkpoint_edges_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_checkpoint_edge();

CREATE OR REPLACE FUNCTION public.cxt_guard_legacy_checkpoint_object() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE stage public.doc_legacy_byte_stages_v1%ROWTYPE;
        source_id uuid;
        source_version bigint;
BEGIN
  IF TG_OP='UPDATE' THEN
    RAISE EXCEPTION 'legacy checkpoint objects are immutable' USING ERRCODE='23514';
  END IF;
  IF TG_OP='DELETE' THEN
    SELECT * INTO stage FROM public.doc_legacy_byte_stages_v1 WHERE doc_hash=OLD.doc_hash;
    IF FOUND THEN
      IF stage.checkpoint_hash IS NULL THEN
        IF COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
          RAISE EXCEPTION 'checkpoint removal requires protocol 1' USING ERRCODE='23514';
        END IF;
      ELSIF COALESCE(current_setting('cxt.legacy_checkpoint_gc_protocol',true),'')<>'1'
        OR OLD.hash=stage.checkpoint_hash
        OR NOT EXISTS(SELECT 1 FROM public.doc_legacy_checkpoint_structures_v1 WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND hash=stage.checkpoint_hash AND kind=2)
        OR NOT EXISTS(SELECT 1 FROM public.doc_legacy_checkpoint_staging_v1 WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND hash=OLD.hash AND last_sequence<stage.checkpoint_sequence)
        OR EXISTS(SELECT 1 FROM public.doc_legacy_checkpoint_edges_v1 WHERE doc_hash=OLD.doc_hash AND stage_id=OLD.stage_id AND child_hash=OLD.hash) THEN
        RAISE EXCEPTION 'current or staged checkpoint objects are pinned' USING ERRCODE='23514';
      END IF;
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

CREATE FUNCTION public.cxt_guard_checkpoint_gc_progress() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.checkpoint_gc_sequence,NEW.checkpoint_gc_last_sequence,NEW.checkpoint_gc_last_hash,NEW.checkpoint_gc_changed,NEW.checkpoint_gc_complete)
     IS DISTINCT FROM (OLD.checkpoint_gc_sequence,OLD.checkpoint_gc_last_sequence,OLD.checkpoint_gc_last_hash,OLD.checkpoint_gc_changed,OLD.checkpoint_gc_complete) THEN
    IF COALESCE(current_setting('cxt.legacy_checkpoint_gc_protocol',true),'')<>'1'
       OR NEW.checkpoint_hash IS NULL OR NEW.checkpoint_gc_sequence<>NEW.checkpoint_sequence
       OR NEW.checkpoint_gc_last_sequence>=NEW.checkpoint_sequence THEN
      RAISE EXCEPTION 'checkpoint reclamation progress requires its current root' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER guard_checkpoint_gc_progress BEFORE UPDATE ON public.doc_legacy_byte_stages_v1
  FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_checkpoint_gc_progress();
