-- 0018: Upgrade original session identifier for snapshot (CIREnvelope.session_origin_id copy).
-- UI (commit list/graph) does not open doc blob and "agent session changed at branch point" (session boundary) meta. Existing rows are '' (excluded from boundary determination).
-- Meta for drawing "changed at branch point" (session boundary). Existing rows are '' (excluded from boundary determination).
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';
