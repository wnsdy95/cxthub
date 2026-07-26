-- Default role to assign to non-members (including anonymous users) in a public workspace.
-- "" | "viewer"(default) | "puller". NULL/unknown value is treated as viewer by the server (fail-closed).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS public_role TEXT;
