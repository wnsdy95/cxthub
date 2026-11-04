CREATE TABLE doc_visible_generations_v1 (
  doc_hash text PRIMARY KEY REFERENCES doc_range_generations_v2(doc_hash) ON DELETE CASCADE,
  source_version bigint NOT NULL CHECK (source_version >= 0),
  range_generation_hash text NOT NULL,
  descriptor_hash text NOT NULL,
  descriptor bytea NOT NULL CHECK (octet_length(descriptor) BETWEEN 1 AND 2048)
);
