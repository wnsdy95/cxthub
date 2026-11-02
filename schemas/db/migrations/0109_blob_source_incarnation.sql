LOCK TABLE blobs IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE blob_source_incarnations (
    hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
    incarnation uuid NOT NULL CHECK (incarnation <> '00000000-0000-0000-0000-000000000000'::uuid)
);

CREATE FUNCTION cxt_assign_blob_incarnation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.incarnation=gen_random_uuid();
    RETURN NEW;
END
$$;

CREATE TRIGGER assign_blob_incarnation BEFORE INSERT ON blob_source_incarnations
FOR EACH ROW EXECUTE FUNCTION cxt_assign_blob_incarnation();

CREATE FUNCTION cxt_preserve_blob_incarnation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'blob source incarnation is immutable' USING ERRCODE='23514';
END
$$;

CREATE TRIGGER preserve_blob_incarnation BEFORE UPDATE ON blob_source_incarnations
FOR EACH ROW EXECUTE FUNCTION cxt_preserve_blob_incarnation();

CREATE FUNCTION cxt_initialize_blob_incarnation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO blob_source_incarnations(hash) VALUES(NEW.hash);
    RETURN NULL;
END
$$;

CREATE TRIGGER initialize_blob_incarnation AFTER INSERT ON blobs
FOR EACH ROW EXECUTE FUNCTION cxt_initialize_blob_incarnation();

INSERT INTO blob_source_incarnations(hash) SELECT hash FROM blobs;
