ALTER TABLE doc_display_jobs_v1
  ADD COLUMN discovery_hash text,
  ADD COLUMN discovery bytea CHECK (octet_length(discovery) BETWEEN 1 AND 65536),
  ADD COLUMN worker_protocol integer NOT NULL DEFAULT 2 CHECK (worker_protocol = 2),
  ADD CONSTRAINT doc_display_discovery_pair CHECK ((discovery_hash IS NULL) = (discovery IS NULL)),
  ADD CONSTRAINT doc_display_discovery_phase CHECK (discovery IS NULL OR
    (source_hash IS NULL AND checkpoint_hash IS NULL AND checkpoint IS NULL));

UPDATE doc_display_jobs_v1
SET state='waiting',version=version+1,lease_until=NULL
WHERE state='running';

CREATE FUNCTION cxthub_guard_doc_display_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.state='running' THEN
    IF TG_OP='INSERT' THEN
      IF COALESCE(current_setting('cxthub.doc_display_protocol',true),'')<>'2' THEN
        RAISE EXCEPTION 'document display jobs require worker protocol 2' USING ERRCODE='0A000';
      END IF;
    ELSIF (OLD.state<>'running' OR NEW.version<>OLD.version)
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'')<>'2' THEN
      RAISE EXCEPTION 'document display jobs require worker protocol 2' USING ERRCODE='0A000';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_display_protocol
  BEFORE INSERT OR UPDATE OF state,version ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_display_protocol();
