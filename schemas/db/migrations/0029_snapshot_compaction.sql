-- 0029: Number of context compression occurrences for snapshot sessions. The compression (compact_boundary) does not change the sessionId.
-- A compression marker (◈) is drawn in the graph for snapshots with a larger value than this one, based on the session_id. S-ID is a meta that does not participate in ID calculations for derived indicators like Parents/grafted. 0 = No compression.
-- Calculates. Derivation metadata (S-ID) not included in ID calculation like Parents/grafted. 0 = No compression.
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS compaction_count int NOT NULL DEFAULT 0;
