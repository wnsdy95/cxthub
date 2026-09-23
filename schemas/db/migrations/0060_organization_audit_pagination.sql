ALTER TABLE organization_audit_events ADD COLUMN IF NOT EXISTS correlation_id text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS organization_audit_cursor_idx ON organization_audit_events(organization_id,created_at DESC,id DESC);
