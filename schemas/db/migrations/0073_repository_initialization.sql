-- Creation proves absence of a per-code Repo, not its containing UI Repository.
-- Legacy observations are independently write-once for each canonical name.
CREATE TABLE repository_initializations (
 repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
 creation jsonb NOT NULL
);
CREATE TABLE repository_initialization_anchors (
 repo_id text NOT NULL REFERENCES repository_initializations(repo_id) ON DELETE CASCADE,
 name text NOT NULL,
 anchor jsonb NOT NULL,
 PRIMARY KEY (repo_id,name)
);
CREATE FUNCTION guard_repository_initialization_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW IS DISTINCT FROM OLD THEN
   RAISE EXCEPTION 'immutable repository initialization receipt' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER repository_initialization_immutable BEFORE UPDATE ON repository_initializations
 FOR EACH ROW EXECUTE FUNCTION guard_repository_initialization_receipt();
CREATE TRIGGER repository_initialization_anchor_immutable BEFORE UPDATE ON repository_initialization_anchors
 FOR EACH ROW EXECUTE FUNCTION guard_repository_initialization_receipt();
