-- 0005: repos.git_remote_url — code repo's git origin (GitHub etc.). Web "Connect" tab link.
ALTER TABLE repos ADD COLUMN IF NOT EXISTS git_remote_url TEXT;
COMMENT ON COLUMN repos.git_remote_url IS 'code repo git origin. cxt reads from local .git to register/push when updated.';
