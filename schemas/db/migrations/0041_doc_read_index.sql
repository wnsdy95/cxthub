-- Rebuildable read projection; archival blobs remain authoritative.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE TABLE doc_read_indexes (
  hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
  version integer NOT NULL,
  envelope jsonb NOT NULL,
  event_count integer NOT NULL
);
-- Inherited prefixes share event text instead of indexing it once per snapshot.
CREATE TABLE doc_search_events (
  hash text PRIMARY KEY,
  search_text text NOT NULL,
  search_lower text NOT NULL
);
CREATE INDEX doc_read_search_trgm ON doc_search_events USING gin(search_lower gin_trgm_ops);
CREATE TABLE doc_read_events (
  doc_hash text NOT NULL REFERENCES doc_read_indexes(hash) ON DELETE CASCADE,
  ordinal integer NOT NULL,
  byte_offset bigint NOT NULL,
  byte_length integer NOT NULL,
  event_hash text NOT NULL REFERENCES doc_search_events(hash),
  seq integer NOT NULL,
  role text NOT NULL,
  PRIMARY KEY(doc_hash,ordinal)
);
CREATE INDEX doc_read_event_locations ON doc_read_events(event_hash,doc_hash);

-- Deleting the last owning document removes its derived searchable text too.
CREATE FUNCTION prune_doc_search_events() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM doc_search_events t WHERE t.hash IN (SELECT event_hash FROM removed_events)
    AND NOT EXISTS (SELECT 1 FROM doc_read_events e WHERE e.event_hash=t.hash);
  RETURN NULL;
END;
$$;
CREATE TRIGGER doc_search_event_cleanup AFTER DELETE ON doc_read_events
REFERENCING OLD TABLE AS removed_events FOR EACH STATEMENT EXECUTE FUNCTION prune_doc_search_events();
