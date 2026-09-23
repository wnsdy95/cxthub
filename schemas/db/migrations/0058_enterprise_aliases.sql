CREATE TABLE IF NOT EXISTS enterprise_slug_aliases (
 slug TEXT PRIMARY KEY,
 enterprise_id TEXT NOT NULL REFERENCES enterprises(id)
);
