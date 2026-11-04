ALTER TABLE doc_range_jobs_v1 ADD COLUMN base_hash text NOT NULL DEFAULT '';
ALTER TABLE doc_range_jobs_v1 DROP CONSTRAINT doc_range_jobs_v1_pkey;
ALTER TABLE doc_range_jobs_v1 ADD PRIMARY KEY (repo_id, doc_hash, base_hash);

CREATE INDEX doc_range_jobs_v1_active_repo ON doc_range_jobs_v1(repo_id)
  WHERE state IN ('waiting','running','retrying');

CREATE TABLE doc_range_boundaries_v1 (
  repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  doc_hash text NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
  base_hash text NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
  algorithm integer NOT NULL CHECK (algorithm = 1),
  doc_source_version bigint NOT NULL CHECK (doc_source_version >= 0),
  base_source_version bigint NOT NULL CHECK (base_source_version >= 0),
  doc_generation_hash text NOT NULL,
  base_generation_hash text NOT NULL,
  job_version bigint NOT NULL CHECK (job_version >= 0),
  events integer NOT NULL CHECK (events >= 0 AND events <= 536870912),
  complete boolean NOT NULL,
  CHECK (complete OR events % 128 = 0),
  PRIMARY KEY (repo_id, doc_hash, base_hash, algorithm)
);
