ALTER TABLE doc_display_sources_v1
  DROP CONSTRAINT doc_display_sources_v1_profile_check,
  ADD CONSTRAINT doc_display_sources_v1_profile_check
    CHECK (profile IN ('tool-result-string-v1','public-event-display-v1'));

ALTER TABLE doc_display_generations_v1
  DROP CONSTRAINT doc_display_generations_v1_profile_check,
  ADD CONSTRAINT doc_display_generations_v1_profile_check
    CHECK (profile IN ('tool-result-string-v1','public-event-display-v1'));

ALTER TABLE doc_display_jobs_v1
  DROP CONSTRAINT doc_display_jobs_v1_profile_check,
  DROP CONSTRAINT doc_display_jobs_v1_worker_protocol_check,
  DROP CONSTRAINT doc_display_jobs_v1_check1,
  ADD COLUMN preparation_hash text,
  ADD COLUMN preparation bytea CHECK (octet_length(preparation) BETWEEN 1 AND 2048),
  ADD CONSTRAINT doc_display_jobs_v1_profile_check
    CHECK (profile IN ('tool-result-string-v1','public-event-display-v1')),
  ADD CONSTRAINT doc_display_jobs_v1_worker_protocol_check
    CHECK ((profile='tool-result-string-v1' AND worker_protocol=2)
        OR (profile='public-event-display-v1' AND worker_protocol=3)),
  ADD CONSTRAINT doc_display_preparation_pair
    CHECK ((preparation_hash IS NULL) = (preparation IS NULL)),
  ADD CONSTRAINT doc_display_preparation_profile
    CHECK ((profile='tool-result-string-v1' AND preparation_hash IS NULL AND preparation IS NULL
        AND ((source_hash IS NULL AND checkpoint_hash IS NULL AND checkpoint IS NULL)
          OR (source_hash IS NOT NULL AND checkpoint_hash IS NOT NULL AND checkpoint IS NOT NULL)))
      OR (profile='public-event-display-v1' AND ((source_hash IS NULL) = (preparation_hash IS NULL))
        AND checkpoint_hash IS NULL AND checkpoint IS NULL
        AND discovery_hash IS NULL AND discovery IS NULL));

CREATE OR REPLACE FUNCTION cxthub_guard_doc_display_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ((NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2)
      OR (NEW.profile='public-event-display-v1' AND NEW.worker_protocol=3)) IS NOT TRUE THEN
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
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'') IN ('2','3') THEN
      RETURN NEW;
    END IF;
    IF NEW.profile='public-event-display-v1' AND NEW.worker_protocol=3
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'')='3' THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'document display jobs require a compatible worker protocol' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER guard_doc_display_protocol ON doc_display_jobs_v1;
CREATE TRIGGER guard_doc_display_protocol
  BEFORE INSERT OR UPDATE OF state,version,profile,worker_protocol ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_display_protocol();
