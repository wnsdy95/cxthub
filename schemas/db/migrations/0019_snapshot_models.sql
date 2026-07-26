-- 0019: Upgrade list of models participating in the snapshot (copy of CIREnvelope.source_models).
-- Meta for drawing the participating AI icon in the UI (commit list) without opening the doc blob.
-- Existing rows are empty arrays (not boundary condition — just omission of display).
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS models TEXT[] NOT NULL DEFAULT '{}';
