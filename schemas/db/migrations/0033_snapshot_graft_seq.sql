-- graft (GraftParents, GraftSeq) LWW register version — join reordering's supersede support.
-- Additive-only grafts create cycles in branch recombination, so a replaceable overlay is needed.
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS graft_seq BIGINT NOT NULL DEFAULT 0;
