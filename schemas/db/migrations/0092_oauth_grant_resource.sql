ALTER TABLE oauth_grants ADD COLUMN resource text NOT NULL DEFAULT '';

CREATE FUNCTION cxt_oauth_grant_resource_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.resource IS DISTINCT FROM OLD.resource THEN
        RAISE EXCEPTION 'OAuth grant resource is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER oauth_grant_resource_immutable BEFORE UPDATE OF resource ON oauth_grants
FOR EACH ROW EXECUTE FUNCTION cxt_oauth_grant_resource_immutable();
