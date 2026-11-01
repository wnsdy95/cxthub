ALTER TABLE doc_range_jobs_v1
  ADD COLUMN legacy_validation_revision integer NOT NULL DEFAULT 0
  CHECK (legacy_validation_revision >= 0);

CREATE FUNCTION cxt_guard_legacy_range_validation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.legacy_validation_revision > 0 AND NEW.state='running'
     AND (TG_OP='INSERT' OR OLD.state<>'running' OR NEW.version<>OLD.version)
     AND COALESCE(current_setting('cxt.legacy_range_validation_revision',true),'')<>NEW.legacy_validation_revision::text
     THEN
    RAISE EXCEPTION 'legacy range claim requires matching source validation revision' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_legacy_range_validation
  BEFORE INSERT OR UPDATE OF state,version,legacy_validation_revision
  ON doc_range_jobs_v1 FOR EACH ROW EXECUTE FUNCTION cxt_guard_legacy_range_validation();
