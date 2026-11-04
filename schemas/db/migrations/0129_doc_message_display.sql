ALTER TABLE doc_display_jobs_v1
  DROP CONSTRAINT doc_display_jobs_v1_worker_protocol_check;

UPDATE doc_display_jobs_v1
SET worker_protocol=4,version=version+1,
  state=CASE WHEN state='running' OR (state='rejected' AND reason='unsupported_display')
    THEN 'waiting' ELSE state END,
  reason=CASE WHEN state='rejected' AND reason='unsupported_display' THEN '' ELSE reason END,
  next_attempt=CASE WHEN state='rejected' AND reason='unsupported_display'
    THEN clock_timestamp() ELSE next_attempt END,
  lease_until=NULL
WHERE profile='public-event-display-v1';

ALTER TABLE doc_display_jobs_v1
  ADD CONSTRAINT doc_display_jobs_v1_worker_protocol_check
    CHECK ((profile='tool-result-string-v1' AND worker_protocol=2)
        OR (profile='public-event-display-v1' AND worker_protocol=4));

CREATE OR REPLACE FUNCTION cxthub_guard_doc_display_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ((NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2)
      OR (NEW.profile='public-event-display-v1' AND NEW.worker_protocol=4)) IS NOT TRUE THEN
    RETURN NEW;
  END IF;
  IF NEW.state='running' THEN
    IF TG_OP='UPDATE' THEN
      IF OLD.state='running' AND NEW.version=OLD.version
        AND NEW.profile=OLD.profile AND NEW.worker_protocol=OLD.worker_protocol THEN
        RETURN NEW;
      END IF;
    END IF;
    IF NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'') IN ('2','3','4') THEN
      RETURN NEW;
    END IF;
    IF NEW.profile='public-event-display-v1' AND NEW.worker_protocol=4
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'')='4' THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'document display jobs require a compatible worker protocol' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;
