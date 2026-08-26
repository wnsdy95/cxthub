-- 0036: Memory component CAS ownership.
--
-- A large MemoryDigest remains identified by the hash of its complete wire
-- JSON, while the at-rest blob may be a manifest whose summary and fragment
-- streams live in 64 KiB component chunks. Keep memory chunks distinct from
-- transcript chunks so each object type's mark-and-sweep cannot delete the
-- other's bodies. Ownership remains repo-scoped.
ALTER TABLE repo_blobs DROP CONSTRAINT IF EXISTS repo_blobs_kind_check;
ALTER TABLE repo_blobs ADD CONSTRAINT repo_blobs_kind_check CHECK (kind IN ('doc', 'memory', 'chunk', 'memory_chunk'));

COMMENT ON COLUMN snapshots.memory_hash IS 'Authoritative pointer to the complete MemoryDigest identity in blobs. The blob may be a storage-only component manifest.';
COMMENT ON TABLE memories IS 'Legacy structured MemoryDigest metadata retained for pointerless snapshots and rolling compatibility; new reads resolve snapshots.memory_hash.';
