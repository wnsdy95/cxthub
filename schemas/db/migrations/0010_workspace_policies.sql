-- 0010: Workspace policy — Minimum role by action.
--
-- secrets_policy  — .cxtsecrets encryption key setting (PUT /repos/{id}/secrets) permission.
-- settings_policy — Team default settings (.claude/.agents) upload permission.
-- Value: ''|'members'(all members, default) | 'owner'(owner only). Forced in app layer.

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS secrets_policy TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS settings_policy TEXT NOT NULL DEFAULT '';
