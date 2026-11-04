CREATE OR REPLACE FUNCTION cxt_storage_blob_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ns text;
BEGIN
 FOR ns IN SELECT namespace_id FROM storage_usage_objects WHERE object_key='blob:'||NEW.hash ORDER BY namespace_id LOOP
 PERFORM cxt_storage_refresh_key(ns,'blob:'||NEW.hash,'payload.raw-binding'); END LOOP;
 RETURN NULL;
END $$;
