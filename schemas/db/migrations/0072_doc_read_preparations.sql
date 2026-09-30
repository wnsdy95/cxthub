-- Invisible, rebuildable read/search blocks can be staged before the repository
-- publication lock. A durable job owns their GC pin, not document access.
CREATE TABLE doc_read_block_preparations_v3 (
  repo_id text NOT NULL,
  job_id text NOT NULL,
  version bigint NOT NULL,
  block_hash text NOT NULL REFERENCES doc_read_blocks_v3(hash),
  -- Set only while retiring successful pins. The FK retains this completion's
  -- exact document index until commit, so its attached blocks need no GC locks.
  published_doc text REFERENCES doc_read_indexes_v3(hash),
  PRIMARY KEY(repo_id,job_id,version,block_hash),
  FOREIGN KEY(repo_id,job_id) REFERENCES doc_finalization_jobs(repo_id,id) ON DELETE CASCADE
);
CREATE INDEX doc_read_block_preparation_owners_v3 ON doc_read_block_preparations_v3(block_hash);

-- Replacing the existing function makes older v3 replicas respect preparation
-- pins too. Last-owner deletion still locks blocks in global hash order before
-- cascading search cleanup. Search rows remain retained by block-event FKs.
CREATE FUNCTION prune_doc_read_block_hashes_v3(hashes text[]) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM b.hash FROM doc_read_blocks_v3 b
    WHERE b.hash=ANY(hashes) ORDER BY b.hash FOR UPDATE;
  DELETE FROM doc_read_blocks_v3 b WHERE b.hash=ANY(hashes)
    AND NOT EXISTS (SELECT 1 FROM doc_read_block_locations_v3 l WHERE l.block_hash=b.hash)
    AND NOT EXISTS (SELECT 1 FROM doc_read_block_preparations_v3 p WHERE p.block_hash=b.hash);
END;
$$;
CREATE OR REPLACE FUNCTION prune_doc_read_blocks_v3() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM prune_doc_read_block_hashes_v3(ARRAY(SELECT block_hash FROM removed_locations));
  RETURN NULL;
END;
$$;
CREATE FUNCTION prune_doc_read_preparations_v3() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- A successful completion has attached these exact blocks and retained that
  -- index via published_doc's FK. Avoid upgrading its block key-share locks:
  -- two simultaneous completions with a shared block would deadlock on upgrade.
  -- All other removals use the ordinary serialized last-owner check.
  PERFORM prune_doc_read_block_hashes_v3(ARRAY(
    SELECT block_hash FROM removed_locations WHERE published_doc IS NULL));
  RETURN NULL;
END;
$$;
CREATE TRIGGER doc_read_preparation_cleanup_v3 AFTER DELETE ON doc_read_block_preparations_v3
REFERENCING OLD TABLE AS removed_locations FOR EACH STATEMENT EXECUTE FUNCTION prune_doc_read_preparations_v3();

-- Renewal preserves pins. Completion/retry/rejection and reclaim retire the old
-- generation, including after process loss; cleanup shares the publication tx.
CREATE FUNCTION retire_doc_read_preparations_v3() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.state='completed' THEN
    UPDATE doc_read_block_preparations_v3 p
      SET published_doc=convert_from(NEW.payload,'UTF8')::jsonb->>'doc_hash'
      WHERE p.repo_id=NEW.repo_id AND p.job_id=NEW.id AND p.version=NEW.version
        AND EXISTS(SELECT 1 FROM doc_read_block_locations_v3 l
          WHERE l.doc_hash=convert_from(NEW.payload,'UTF8')::jsonb->>'doc_hash'
            AND l.block_hash=p.block_hash);
  END IF;
  DELETE FROM doc_read_block_preparations_v3
    WHERE repo_id=NEW.repo_id AND job_id=NEW.id
      AND (version<>NEW.version OR NEW.state<>'running');
  RETURN NULL;
END;
$$;
CREATE TRIGGER doc_job_preparation_retirement AFTER UPDATE ON doc_finalization_jobs
FOR EACH ROW WHEN (OLD.version IS DISTINCT FROM NEW.version OR OLD.state IS DISTINCT FROM NEW.state)
EXECUTE FUNCTION retire_doc_read_preparations_v3();
