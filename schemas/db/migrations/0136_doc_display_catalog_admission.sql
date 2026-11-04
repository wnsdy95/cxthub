LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1 IN SHARE ROW EXCLUSIVE MODE NOWAIT;

CREATE TABLE doc_display_catalog_admissions_v1 (
  event_hash text NOT NULL,
  profile text NOT NULL,
  source_hash text,
  generation uuid NOT NULL DEFAULT gen_random_uuid(),
  admission_revision bigint NOT NULL DEFAULT 0 CHECK (admission_revision >= 0),
  PRIMARY KEY (event_hash,profile)
);

INSERT INTO doc_display_catalog_admissions_v1(event_hash,profile,source_hash)
SELECT event_hash,profile,source_hash FROM doc_display_sources_v1;

INSERT INTO doc_display_catalog_admissions_v1(event_hash,profile)
SELECT DISTINCT event_hash,profile FROM doc_display_jobs_v1
ON CONFLICT DO NOTHING;

CREATE FUNCTION cxthub_lock_doc_catalog_affinities(native_events text[],native_profiles text[],source_hashes text[])
RETURNS TABLE(event_hash text,profile text) LANGUAGE plpgsql AS $$
DECLARE
  owner record;
  fence record;
BEGIN
  FOR owner IN
    SELECT source.event_hash,source.profile FROM doc_display_sources_v1 source
    WHERE source.source_hash=ANY(source_hashes) ORDER BY source.source_hash FOR KEY SHARE NOWAIT
  LOOP
    native_events=array_append(native_events,owner.event_hash);
    native_profiles=array_append(native_profiles,owner.profile);
  END LOOP;
  FOR fence IN
    SELECT DISTINCT hashtextextended('cxthub-doc-catalog-affinity:'||input.event_hash||':'||input.profile,0) AS identity
    FROM unnest(native_events,native_profiles) input(event_hash,profile)
    WHERE input.event_hash IS NOT NULL AND input.profile IS NOT NULL ORDER BY identity
  LOOP
    IF NOT pg_try_advisory_xact_lock(fence.identity) THEN
      RAISE EXCEPTION 'document catalog admission is busy' USING ERRCODE='55P03';
    END IF;
  END LOOP;
  RETURN QUERY SELECT DISTINCT input.event_hash,input.profile
    FROM unnest(native_events,native_profiles) input(event_hash,profile)
    WHERE input.event_hash IS NOT NULL AND input.profile IS NOT NULL
    ORDER BY input.event_hash,input.profile;
END;
$$;

CREATE FUNCTION cxthub_record_doc_catalog_job_admission() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_event text;
  previous_profile text;
  previous_source text;
  next_event text;
  next_profile text;
  next_source text;
  affinity record;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.profile,NEW.source_hash,NEW.state='completed')
      IS NOT DISTINCT FROM (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.profile,OLD.source_hash,OLD.state='completed') THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_event=OLD.event_hash;
    previous_profile=OLD.profile;
    previous_source=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_event=NEW.event_hash;
    next_profile=NEW.profile;
    next_source=NEW.source_hash;
  END IF;
  FOR affinity IN SELECT * FROM cxthub_lock_doc_catalog_affinities(
    ARRAY[previous_event,next_event],ARRAY[previous_profile,next_profile],ARRAY[previous_source,next_source])
  LOOP
    INSERT INTO doc_display_catalog_admissions_v1 AS admission(event_hash,profile,admission_revision)
    VALUES(affinity.event_hash,affinity.profile,1)
    ON CONFLICT(event_hash,profile) DO UPDATE SET admission_revision=admission.admission_revision+1;
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_display_jobs_v1_z_catalog_admission
  AFTER INSERT OR UPDATE OF repo_id,doc_hash,event_index,event_hash,profile,source_hash,state OR DELETE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_record_doc_catalog_job_admission();

CREATE FUNCTION cxthub_record_doc_catalog_source() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_event text;
  previous_profile text;
  next_event text;
  next_profile text;
  affinity record;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.event_hash,NEW.profile,NEW.source_hash,NEW.bytes)
      IS NOT DISTINCT FROM (OLD.event_hash,OLD.profile,OLD.source_hash,OLD.bytes) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_event=OLD.event_hash;
    previous_profile=OLD.profile;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_event=NEW.event_hash;
    next_profile=NEW.profile;
  END IF;
  FOR affinity IN SELECT * FROM cxthub_lock_doc_catalog_affinities(
    ARRAY[previous_event,next_event],ARRAY[previous_profile,next_profile],ARRAY[]::text[])
  LOOP
    INSERT INTO doc_display_catalog_admissions_v1(event_hash,profile)
    VALUES(affinity.event_hash,affinity.profile) ON CONFLICT DO NOTHING;
  END LOOP;
  IF TG_OP<>'INSERT' THEN
    UPDATE doc_display_catalog_admissions_v1 SET source_hash=NULL,generation=gen_random_uuid()
    WHERE event_hash=previous_event AND profile=previous_profile AND source_hash=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    INSERT INTO doc_display_catalog_admissions_v1(event_hash,profile,source_hash)
    VALUES(next_event,next_profile,NEW.source_hash)
    ON CONFLICT(event_hash,profile) DO UPDATE SET source_hash=EXCLUDED.source_hash,generation=EXCLUDED.generation;
  END IF;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_display_sources_v1_catalog_admission
  AFTER INSERT OR UPDATE OF event_hash,profile,source_hash,bytes OR DELETE ON doc_display_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_record_doc_catalog_source();
