LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1 IN SHARE ROW EXCLUSIVE MODE NOWAIT;

CREATE INDEX doc_display_jobs_v1_catalog_native
  ON doc_display_jobs_v1(event_hash,profile,repo_id,doc_hash,event_index);

CREATE TABLE doc_display_catalog_census_v1 (
  source_hash text PRIMARY KEY REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  generation uuid NOT NULL,
  admission_revision bigint NOT NULL CHECK (admission_revision >= 0),
  source_revision bigint NOT NULL CHECK (source_revision >= 0),
  phase text NOT NULL CHECK (phase IN ('native','explicit','protected','clear')),
  reason text NOT NULL DEFAULT '' CHECK (reason IN ('','resumable_job','unverified_completion')),
  after_repo text NOT NULL DEFAULT '',
  after_doc text NOT NULL DEFAULT '',
  after_event integer NOT NULL DEFAULT -1 CHECK (after_event >= -1),
  after_profile text NOT NULL DEFAULT '',
  CHECK ((phase='protected')=(reason<>''))
);

CREATE OR REPLACE FUNCTION cxthub_queue_doc_display_job_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  next_source text;
  previous_event text;
  next_event text;
  previous_profile text;
  next_profile text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.event_length,NEW.profile,NEW.worker_protocol,NEW.source_hash,NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation,NEW.discovery_hash,NEW.discovery)
      IS NOT DISTINCT FROM (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.event_length,OLD.profile,OLD.worker_protocol,OLD.source_hash,OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation,OLD.discovery_hash,OLD.discovery) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
    previous_event=OLD.event_hash;
    previous_profile=OLD.profile;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
    next_event=NEW.event_hash;
    next_profile=NEW.profile;
  END IF;
  FOR queued_source IN
    SELECT roots.source_hash FROM (
      SELECT unnest(ARRAY[previous_source,next_source]) AS source_hash
      UNION
      SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=previous_event AND profile=previous_profile
      UNION
      SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=next_event AND profile=next_profile
    ) roots WHERE roots.source_hash IS NOT NULL ORDER BY roots.source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

DROP TRIGGER doc_display_jobs_v1_queue_roots ON doc_display_jobs_v1;

CREATE TRIGGER doc_display_jobs_v1_queue_roots
  AFTER INSERT OR UPDATE OF repo_id,doc_hash,event_index,event_hash,event_length,profile,worker_protocol,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,discovery_hash,discovery OR DELETE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_job_roots();

CREATE FUNCTION cxthub_wake_doc_catalog_census() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  queued_source text;
BEGIN
  FOR queued_source IN
    SELECT DISTINCT source.source_hash FROM cxthub_lock_doc_catalog_affinities(
      ARRAY[OLD.event_hash,NEW.event_hash],ARRAY[OLD.profile,NEW.profile],ARRAY[OLD.source_hash,NEW.source_hash]) affinity
    JOIN doc_display_sources_v1 source ON source.event_hash=affinity.event_hash AND source.profile=affinity.profile
    ORDER BY source.source_hash
  LOOP
    UPDATE doc_display_maintenance_v1 SET
      phase=CASE WHEN phase='idle' THEN 'index' ELSE phase END,
      due_at=LEAST(due_at,clock_timestamp())
    WHERE source_hash=queued_source;
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_display_jobs_v1_zz_catalog_census
  AFTER UPDATE OF state ON doc_display_jobs_v1
  FOR EACH ROW WHEN ((NEW.state='completed') IS DISTINCT FROM (OLD.state='completed'))
  EXECUTE FUNCTION cxthub_wake_doc_catalog_census();

INSERT INTO doc_display_maintenance_v1(source_hash) SELECT source_hash FROM doc_display_sources_v1
ON CONFLICT(source_hash) DO UPDATE SET phase=CASE WHEN doc_display_maintenance_v1.phase='idle' THEN 'index' ELSE doc_display_maintenance_v1.phase END,
  due_at=LEAST(doc_display_maintenance_v1.due_at,clock_timestamp());
