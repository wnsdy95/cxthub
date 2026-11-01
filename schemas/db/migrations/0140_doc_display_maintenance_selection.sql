SET LOCAL lock_timeout='250ms';
LOCK TABLE doc_display_maintenance_v1 IN SHARE ROW EXCLUSIVE MODE NOWAIT;

CREATE TABLE doc_display_maintenance_cursor_v1 (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  after_source text NOT NULL DEFAULT ''
);

INSERT INTO doc_display_maintenance_cursor_v1(singleton) VALUES(true);

CREATE INDEX doc_display_maintenance_v1_ring ON doc_display_maintenance_v1(source_hash)
  WHERE phase<>'idle';

CREATE INDEX doc_display_generations_v1_source ON doc_display_generations_v1(source_hash);
