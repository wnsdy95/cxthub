SET LOCAL lock_timeout='250ms';
LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1 IN SHARE ROW EXCLUSIVE MODE NOWAIT;

CREATE FUNCTION cxthub_guard_doc_catalog_job_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.state='running' OR NEW.state='running') AND
    (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.profile) IS DISTINCT FROM
    (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.profile) THEN
    RAISE EXCEPTION 'running document display job identity is immutable' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_display_a_catalog_identity
  BEFORE UPDATE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_job_identity();

CREATE FUNCTION cxthub_guard_doc_catalog_admission_order() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_event text;
  previous_profile text;
  previous_source text;
  next_event text;
  next_profile text;
  next_source text;
  locked_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.event_length,NEW.profile,NEW.worker_protocol,
        NEW.source_hash,NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation,
        NEW.discovery_hash,NEW.discovery,NEW.state='completed') IS NOT DISTINCT FROM
       (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.event_length,OLD.profile,OLD.worker_protocol,
        OLD.source_hash,OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation,
        OLD.discovery_hash,OLD.discovery,OLD.state='completed') THEN
      RETURN NEW;
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
  PERFORM * FROM cxthub_lock_doc_catalog_affinities(
    ARRAY[previous_event,next_event],ARRAY[previous_profile,next_profile],ARRAY[previous_source,next_source]);
  FOR locked_source IN
    SELECT owners.source_hash FROM (
      SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=previous_event AND profile=previous_profile
      UNION
      SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=next_event AND profile=next_profile
      UNION
      SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=ANY(ARRAY[previous_source,next_source])
    ) owners ORDER BY owners.source_hash
  LOOP
    PERFORM source_hash FROM doc_display_sources_v1 WHERE source_hash=locked_source FOR NO KEY UPDATE;
  END LOOP;
  IF TG_OP='DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_display_jobs_v1_a_catalog_admission_order
  AFTER INSERT OR UPDATE OR DELETE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_admission_order();
