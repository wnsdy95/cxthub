-- P6 preparation only. Does not enable root publication or rewrite history.
ALTER TABLE repos ADD COLUMN required_doc_identity text NOT NULL DEFAULT ''
    CHECK (required_doc_identity IN ('', 'cxt-manifest-sha256-v1'));
CREATE FUNCTION cxt_repository_doc_identity_monotonic() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.required_doc_identity <> '' AND NEW.required_doc_identity IS DISTINCT FROM OLD.required_doc_identity THEN
        RAISE EXCEPTION 'repository document identity requirement cannot be downgraded' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER repository_doc_identity_monotonic BEFORE UPDATE OF required_doc_identity ON repos
FOR EACH ROW EXECUTE FUNCTION cxt_repository_doc_identity_monotonic();
