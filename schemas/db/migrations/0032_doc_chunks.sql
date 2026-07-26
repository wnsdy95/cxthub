-- 0032: Session doc chunk CAS — repo_blobs.kind allows 'chunk'.
--
-- doc is a comprehensive transcript object up to the capture point, so it is
-- rewritten every time the session grows (actual duplication 97%). To chunkify the storage layer but keep the integrity hash (doc hash) unchanged:
--   blobs(doc hash)   = manifest{format, envelope, chunks[]}  (legacy comprehensive and mixed chunk allowed)
--   blobs(chunk hash) = event canonical fragment
-- It also isolates chunk ownership in repo_blobs (kind='chunk') — maintaining content access blocking between repos.
ALTER TABLE repo_blobs DROP CONSTRAINT IF EXISTS repo_blobs_kind_check;
ALTER TABLE repo_blobs ADD CONSTRAINT repo_blobs_kind_check CHECK (kind IN ('doc', 'memory', 'chunk'));
