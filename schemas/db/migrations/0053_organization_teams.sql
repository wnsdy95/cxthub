CREATE TABLE teams (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    name TEXT NOT NULL,
    slug TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(organization_id,slug),
    UNIQUE(id,organization_id)
);
CREATE TABLE team_memberships (
    team_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK(role IN ('member','maintainer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(team_id,user_id),
    FOREIGN KEY(team_id,organization_id) REFERENCES teams(id,organization_id) ON DELETE CASCADE,
    FOREIGN KEY(organization_id,user_id) REFERENCES organization_memberships(organization_id,user_id) ON DELETE CASCADE
);
CREATE INDEX team_memberships_user ON team_memberships(user_id,team_id);
CREATE TABLE team_repository_grants (
    team_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    repository_id TEXT NOT NULL REFERENCES repositories(id),
    role TEXT NOT NULL CHECK(role IN ('viewer','puller','member','maintainer','owner')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(team_id,repository_id),
    FOREIGN KEY(team_id,organization_id) REFERENCES teams(id,organization_id) ON DELETE CASCADE
);
CREATE INDEX team_repository_grants_repository ON team_repository_grants(repository_id,team_id);
