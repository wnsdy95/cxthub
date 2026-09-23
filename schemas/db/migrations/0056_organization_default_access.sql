ALTER TABLE organization_policies
ADD COLUMN default_repository_role TEXT NOT NULL DEFAULT ''
CHECK (default_repository_role IN ('', 'viewer', 'puller', 'member', 'maintainer', 'owner'));
