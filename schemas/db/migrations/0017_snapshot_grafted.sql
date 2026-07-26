-- 0017: diverged append(graft) marker — cxt push --append appends to the root of the lineage
-- when the server records it. It is a derivative commit meta without the content-hash, like parents,
-- used by the UI (commit graph/list) to display the "merge point of another context session".
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS grafted BOOLEAN NOT NULL DEFAULT false;
