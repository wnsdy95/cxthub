-- 0030: graft reachability overlay parent. Destructively appends (graft) when diverged
-- Overwrites (formerly SetSnapshotParents) the same snapshot ID, causing local/server parents to diverge on replica
-- Agreement broke and sync conflict is permanentized. Now parents are immutable, head is here only
-- Appends while reachability is calculated as parents ∪ graft_parents. ID calculation excluded (S-ID).
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS graft_parents text[] NOT NULL DEFAULT '{}';
