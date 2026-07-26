-- 0012: GitHub public status sync (GitHub → cxthub one-way).
-- When enabled, visibility manual setting is locked, and only when all linked GitHub repos are public.
-- (Note: Workspaces upsert also updates owner_id for ownership transfer — no schema change).

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS gh_visibility_sync BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS gh_synced_at TIMESTAMPTZ;
