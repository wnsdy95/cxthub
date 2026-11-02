CREATE TABLE doc_display_sources_v1 (
  event_hash text NOT NULL,
  profile text NOT NULL CHECK (profile = 'tool-result-string-v1'),
  source_hash text NOT NULL UNIQUE,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  PRIMARY KEY (event_hash, profile)
);

CREATE TABLE doc_display_objects_v1 (
  source_hash text NOT NULL REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 131072),
  PRIMARY KEY (source_hash, hash)
);

CREATE TABLE doc_display_generations_v1 (
  event_hash text NOT NULL,
  profile text NOT NULL CHECK (profile = 'tool-result-string-v1'),
  source_hash text NOT NULL REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  generation_hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  PRIMARY KEY (event_hash, profile),
  FOREIGN KEY (event_hash, profile) REFERENCES doc_display_sources_v1(event_hash, profile) ON DELETE CASCADE
);

CREATE TABLE doc_display_jobs_v1 (
  repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  doc_hash text NOT NULL,
  profile text NOT NULL CHECK (profile = 'tool-result-string-v1'),
  event_index integer NOT NULL CHECK (event_index >= 0),
  event_hash text NOT NULL,
  event_offset bigint NOT NULL CHECK (event_offset >= 0),
  event_length bigint NOT NULL CHECK (event_length > 0),
  event_seq bigint NOT NULL,
  event_role text NOT NULL CHECK (octet_length(event_role) <= 64),
  source_id uuid NOT NULL,
  source_version bigint NOT NULL CHECK (source_version >= 0),
  range_generation text NOT NULL,
  state text NOT NULL CHECK (state IN ('waiting','running','retrying','completed','rejected')),
  reason text NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 128),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
  created_at timestamptz NOT NULL,
  next_attempt timestamptz NOT NULL,
  lease_until timestamptz,
  source_hash text REFERENCES doc_display_sources_v1(source_hash),
  checkpoint_hash text,
  checkpoint bytea CHECK (octet_length(checkpoint) BETWEEN 1 AND 65536),
  PRIMARY KEY (repo_id, doc_hash, event_index, profile),
  CHECK ((state = 'running') = (lease_until IS NOT NULL)),
  CHECK ((source_hash IS NULL AND checkpoint_hash IS NULL AND checkpoint IS NULL)
      OR (source_hash IS NOT NULL AND checkpoint_hash IS NOT NULL AND checkpoint IS NOT NULL))
);

CREATE INDEX doc_display_jobs_v1_due ON doc_display_jobs_v1(next_attempt, created_at, repo_id, doc_hash, event_index)
  WHERE state IN ('waiting','running','retrying');
CREATE INDEX doc_display_jobs_v1_active_repo ON doc_display_jobs_v1(repo_id)
  WHERE state IN ('waiting','running','retrying');
CREATE UNIQUE INDEX doc_display_jobs_v1_one_running_per_repo ON doc_display_jobs_v1(repo_id)
  WHERE state = 'running';
