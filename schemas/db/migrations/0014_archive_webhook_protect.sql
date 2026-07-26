-- 0014: archive · webhook · protect branch · (slug is existing column — manual editing only).
--
-- workspaces.archived     — read-only archive (no P1 delete). viewer write over 403.
-- workspaces.webhook_url  — Slack incoming webhook compatibility ({"text"} POST), async invocation on ref refresh.
-- repos.protect_default   — Default branch protection --force ref movement denied (protected branch).

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS archived BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS webhook_url TEXT NOT NULL DEFAULT '';

ALTER TABLE repos
  ADD COLUMN IF NOT EXISTS protect_default BOOLEAN NOT NULL DEFAULT FALSE;
