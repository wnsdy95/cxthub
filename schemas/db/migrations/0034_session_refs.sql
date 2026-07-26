-- 0034_session_refs.sql
-- The remaining natural descendants from a partial join are not actual git branches. They are treated as branch kinds.
-- Preserving it would block cross-Git-branch joins until that session branch was recombined.
-- An internal session ref that only preserves reachability is treated as a separate kind.
-- The new name follows the format fork/v1/<git-branch-byte-length>/<git-branch>/<short-tip>.
-- The length component prevents prefix scope conflicts between feature and feature/foo. The legacy name is
-- It is read for preserving reachability but is not used as evidence for the same-branch permission of a new join.

ALTER TABLE refs DROP CONSTRAINT IF EXISTS refs_kind_check;
ALTER TABLE refs
    ADD CONSTRAINT refs_kind_check CHECK (kind IN ('head', 'branch', 'session', 'tag'));

COMMENT ON COLUMN refs.kind IS
    'head=Current checkout symbolic ref | branch=Actual git branch | session=Remaining session branches from a partial join | tag=Immutable label';
