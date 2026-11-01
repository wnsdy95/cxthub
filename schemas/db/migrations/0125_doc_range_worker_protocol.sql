CREATE FUNCTION cxt_guard_doc_range_worker_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.state='running' AND (OLD.state<>'running' OR NEW.version<>OLD.version)
     AND COALESCE(current_setting('cxt.doc_range_worker_protocol', true),'')<>'3'
     AND EXISTS (
       SELECT 1 FROM snapshots
       WHERE repo_id=NEW.repo_id AND id IN (NEW.doc_hash,NEW.base_hash)
         AND doc_hash=id AND doc_identity=''
     ) THEN
    RAISE EXCEPTION 'legacy document range jobs require worker protocol 3'
      USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_range_worker_protocol
  BEFORE UPDATE OF state,version ON doc_range_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxt_guard_doc_range_worker_protocol();
