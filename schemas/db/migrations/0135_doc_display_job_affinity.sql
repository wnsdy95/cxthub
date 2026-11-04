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
    IF (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.profile,NEW.source_hash,NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation)
      IS NOT DISTINCT FROM (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.profile,OLD.source_hash,OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation) THEN
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
  AFTER INSERT OR UPDATE OF repo_id,doc_hash,event_index,event_hash,profile,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation OR DELETE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_job_roots();
