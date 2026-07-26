-- 0013: Drop policy allowlists — Role ladder with 5 levels of user segmentation
-- (viewer/puller/member/maintainer/owner) replaces it. Policy is ''|'members'|'owner' only.

ALTER TABLE workspaces
  DROP COLUMN IF EXISTS secrets_allowed,
  DROP COLUMN IF EXISTS settings_allowed;
