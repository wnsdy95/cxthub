-- Historical connection paths remain reserved across rename/transfer. Keep both
-- the namespace key and literal owner handle so old CLI URLs survive a rename.
CREATE FUNCTION cxt_reserve_repository_path() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text; target text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM ownership_migrations WHERE version='repository-ownership-v1') THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND COALESCE(OLD.slug,'')<>'' AND COALESCE(OLD.owner_username,'')<>'' THEN
  FOREACH k IN ARRAY ARRAY[COALESCE(NULLIF(OLD.owner_namespace_id,''),'handle:'||OLD.owner_username),'handle:'||OLD.owner_username] LOOP
   target:=NULL;
   INSERT INTO repository_path_aliases AS a(namespace_key,owner_handle,path,repository_id)
    VALUES(k,OLD.owner_username,OLD.slug,OLD.id)
    ON CONFLICT(namespace_key,path) DO UPDATE SET repository_id=a.repository_id
    WHERE a.repository_id=EXCLUDED.repository_id RETURNING repository_id INTO target;
   IF target IS NULL THEN RAISE EXCEPTION 'historical repository address conflicts' USING ERRCODE='23505'; END IF;
  END LOOP;
 END IF;
 IF COALESCE(NEW.slug,'')<>'' AND COALESCE(NEW.owner_username,'')<>'' THEN
  FOREACH k IN ARRAY ARRAY[COALESCE(NULLIF(NEW.owner_namespace_id,''),'handle:'||NEW.owner_username),'handle:'||NEW.owner_username] LOOP
   target:=NULL;
   INSERT INTO repository_path_aliases AS a(namespace_key,owner_handle,path,repository_id)
    VALUES(k,NEW.owner_username,NEW.slug,NEW.id)
    ON CONFLICT(namespace_key,path) DO UPDATE SET repository_id=a.repository_id
    WHERE a.repository_id=EXCLUDED.repository_id RETURNING repository_id INTO target;
   IF target IS NULL THEN RAISE EXCEPTION 'repository address is reserved' USING ERRCODE='23505'; END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER repository_path_reservations AFTER INSERT OR UPDATE ON repositories
 FOR EACH ROW EXECUTE FUNCTION cxt_reserve_repository_path();
