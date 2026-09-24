CREATE TABLE github_connections (
 id text PRIMARY KEY REFERENCES namespaces(id),
 body jsonb NOT NULL,
 CHECK (body->>'namespace_id'=id),
 CHECK ((body->'installation'->>'id')::bigint>0)
);
CREATE UNIQUE INDEX github_installation_owner ON github_connections ((body->'installation'->>'id'));
CREATE TABLE github_identities (
 id text PRIMARY KEY REFERENCES users(id),
 body jsonb NOT NULL,
 CHECK (body->>'user_id'=id),
 CHECK ((body->>'external_id')::bigint>0)
);
CREATE UNIQUE INDEX github_external_identity ON github_identities ((body->>'external_id'));
CREATE TABLE github_requests (id text PRIMARY KEY, body jsonb NOT NULL);
CREATE TABLE github_deliveries (id text PRIMARY KEY, body jsonb NOT NULL);
CREATE TABLE github_team_members (
 namespace_id text NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 organization_id text NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(namespace_id,team_id,user_id),
 FOREIGN KEY(organization_id,user_id) REFERENCES organization_memberships(organization_id,user_id) ON DELETE CASCADE
);
CREATE INDEX github_team_members_user ON github_team_members(user_id,team_id);
-- Manual membership wins; imported grants never overwrite its role.
CREATE VIEW effective_team_memberships AS
 SELECT team_id,organization_id,user_id,role,created_at,''::text AS source FROM team_memberships
 UNION ALL
 SELECT g.team_id,t.organization_id,g.user_id,'member'::text,g.expires_at - interval '10 minutes','github'::text
 FROM github_team_members g
 JOIN teams t ON t.id=g.team_id
 JOIN organizations o ON o.id=t.organization_id AND o.namespace_id=g.namespace_id
 JOIN organization_memberships om ON om.organization_id=t.organization_id AND om.user_id=g.user_id
 JOIN github_connections c ON c.id=g.namespace_id
 WHERE g.expires_at>now() AND (c.body->>'enabled')::boolean AND c.body->>'status'='connected'
 AND NOT EXISTS (SELECT 1 FROM team_memberships m WHERE m.team_id=g.team_id AND m.user_id=g.user_id);
CREATE INDEX github_delivery_waiting ON github_deliveries(id) WHERE NOT (body->>'Done')::boolean;
