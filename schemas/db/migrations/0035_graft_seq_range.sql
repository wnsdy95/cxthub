-- 0035_graft_seq_range.sql
-- graft_seq is a non-negative Lamport version shared with FS/CLI. It increments from the BIGINT upper bound without advancing.
-- It fails closed if the application does not advance, and it prevents seq=0 legacy merge rules from reverting.
-- This ensures that seq=0 legacy merge rules are not reverted.

ALTER TABLE snapshots
    ADD CONSTRAINT snapshots_graft_seq_nonnegative CHECK (graft_seq >= 0);
