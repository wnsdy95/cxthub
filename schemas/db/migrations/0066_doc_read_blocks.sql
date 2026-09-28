-- Rebuildable event-location blocks. Existing v2 projections remain readable
-- during rolling upgrades; neither canonical archives nor content IDs change.
CREATE TABLE doc_read_indexes_v3 (
  hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
  version integer NOT NULL CHECK(version=1),
  envelope jsonb NOT NULL,
  event_count integer NOT NULL CHECK(event_count>=0)
);
CREATE TABLE doc_read_blocks_v3 (
  hash text PRIMARY KEY,
  event_count integer NOT NULL CHECK(event_count BETWEEN 1 AND 128)
);
CREATE TABLE doc_read_block_events_v3 (
  block_hash text NOT NULL REFERENCES doc_read_blocks_v3(hash) ON DELETE CASCADE,
  ordinal integer NOT NULL CHECK(ordinal>=0),
  byte_offset bigint NOT NULL CHECK(byte_offset>=0),
  byte_length integer NOT NULL CHECK(byte_length>0),
  event_hash text NOT NULL REFERENCES doc_search_events_v2(hash),
  seq integer NOT NULL,
  role text NOT NULL,
  PRIMARY KEY(block_hash,ordinal)
);
CREATE INDEX doc_read_block_events_search_v3 ON doc_read_block_events_v3(event_hash,block_hash);
CREATE TABLE doc_read_block_locations_v3 (
  doc_hash text NOT NULL REFERENCES doc_read_indexes_v3(hash) ON DELETE CASCADE,
  first_event integer NOT NULL CHECK(first_event>=0),
  byte_offset bigint NOT NULL CHECK(byte_offset>=0),
  block_hash text NOT NULL REFERENCES doc_read_blocks_v3(hash),
  PRIMARY KEY(doc_hash,first_event)
);
CREATE INDEX doc_read_block_owners_v3 ON doc_read_block_locations_v3(block_hash,doc_hash);

-- Search text is shared by both projection versions. An older replica may
-- create/delete v2 indexes after this migration, so both cleanup paths honor
-- ownership by either representation.
CREATE OR REPLACE FUNCTION prune_doc_search_events_v2() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- Serialize ownership decisions, then use a fresh READ COMMITTED statement.
  -- Otherwise two concurrent last-owner deletions can each see the other's
  -- uncommitted reference and both leave an unowned search row behind.
  PERFORM t.hash FROM doc_search_events_v2 t
    WHERE t.hash IN (SELECT event_hash FROM removed_events)
    ORDER BY t.hash FOR UPDATE;
  DELETE FROM doc_search_events_v2 t WHERE t.hash IN (SELECT event_hash FROM removed_events)
    AND NOT EXISTS (SELECT 1 FROM doc_read_events_v2 e WHERE e.event_hash=t.hash)
    AND NOT EXISTS (SELECT 1 FROM doc_read_block_events_v3 e WHERE e.event_hash=t.hash);
  RETURN NULL;
END;
$$;
CREATE TRIGGER doc_read_block_text_cleanup_v3 AFTER DELETE ON doc_read_block_events_v3
REFERENCING OLD TABLE AS removed_events FOR EACH STATEMENT EXECUTE FUNCTION prune_doc_search_events_v2();

CREATE FUNCTION prune_doc_read_blocks_v3() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM b.hash FROM doc_read_blocks_v3 b
    WHERE b.hash IN (SELECT block_hash FROM removed_locations)
    ORDER BY b.hash FOR UPDATE;
  DELETE FROM doc_read_blocks_v3 b WHERE b.hash IN (SELECT block_hash FROM removed_locations)
    AND NOT EXISTS (SELECT 1 FROM doc_read_block_locations_v3 l WHERE l.block_hash=b.hash);
  RETURN NULL;
END;
$$;
CREATE TRIGGER doc_read_block_cleanup_v3 AFTER DELETE ON doc_read_block_locations_v3
REFERENCING OLD TABLE AS removed_locations FOR EACH STATEMENT EXECUTE FUNCTION prune_doc_read_blocks_v3();

CREATE VIEW doc_read_index_current AS
  SELECT hash,version,envelope,event_count FROM doc_read_indexes_v3
  UNION ALL
  SELECT i.hash,i.version,i.envelope,i.event_count FROM doc_read_indexes_v2 i
    WHERE NOT EXISTS(SELECT 1 FROM doc_read_indexes_v3 n WHERE n.hash=i.hash);
CREATE VIEW doc_read_event_locations_current AS
  SELECT l.doc_hash,l.first_event+e.ordinal AS ordinal,
    l.byte_offset+e.byte_offset AS byte_offset,e.byte_length,e.event_hash,e.seq,e.role
  FROM doc_read_block_locations_v3 l JOIN doc_read_block_events_v3 e ON e.block_hash=l.block_hash
  UNION ALL
  SELECT e.doc_hash,e.ordinal,e.byte_offset,e.byte_length,e.event_hash,e.seq,e.role FROM doc_read_events_v2 e
    WHERE NOT EXISTS(SELECT 1 FROM doc_read_indexes_v3 n WHERE n.hash=e.doc_hash);
