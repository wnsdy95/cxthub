-- The former company-space records have been migrated to organizations in
-- 0052. These new enterprise accounts are explicitly created, never inferred.
CREATE TABLE enterprises (
 id text PRIMARY KEY,
 slug text UNIQUE NOT NULL,
 record jsonb NOT NULL
);
CREATE TABLE enterprise_memberships (
 enterprise_id text NOT NULL REFERENCES enterprises(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES users(id),
 role text NOT NULL CHECK(role IN ('owner','admin','member')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(enterprise_id,user_id)
);
CREATE INDEX enterprise_member_user ON enterprise_memberships(user_id);
CREATE TABLE enterprise_organizations (
 organization_id text PRIMARY KEY REFERENCES organizations(id),
 enterprise_id text NOT NULL REFERENCES enterprises(id)
);
CREATE INDEX enterprise_organization_parent ON enterprise_organizations(enterprise_id);
CREATE TABLE enterprise_audit_events (
 id text PRIMARY KEY,
 enterprise_id text NOT NULL REFERENCES enterprises(id),
 actor_id text NOT NULL,
 action text NOT NULL,
 target_id text NOT NULL,
 created_at timestamptz NOT NULL
);
CREATE INDEX enterprise_audit_recent ON enterprise_audit_events(enterprise_id,created_at DESC,id DESC);
CREATE FUNCTION cxt_require_enterprise_account_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE eid text;
BEGIN
 eid:=COALESCE(NEW.enterprise_id,OLD.enterprise_id);
 PERFORM pg_advisory_xact_lock(hashtextextended(eid,2));
 IF EXISTS(SELECT 1 FROM enterprises WHERE id=eid) AND NOT EXISTS(SELECT 1 FROM enterprise_memberships WHERE enterprise_id=eid AND role='owner') THEN
  RAISE EXCEPTION 'enterprise requires an owner' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER enterprise_account_owner AFTER INSERT OR UPDATE OR DELETE ON enterprise_memberships
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_require_enterprise_account_owner();
