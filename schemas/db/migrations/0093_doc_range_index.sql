CREATE TABLE doc_range_indexes_v1 (
  hash text PRIMARY KEY REFERENCES blobs(hash) ON DELETE CASCADE,
  header bytea NOT NULL CHECK (octet_length(header) BETWEEN 1 AND 33554432)
);

CREATE TABLE doc_range_pages_v1 (
  doc_hash text NOT NULL REFERENCES doc_range_indexes_v1(hash) ON DELETE CASCADE,
  ordinal integer NOT NULL CHECK (ordinal >= 0 AND ordinal < (536870912 + 127) / 128),
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 131072),
  PRIMARY KEY (doc_hash, ordinal)
);
