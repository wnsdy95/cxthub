SET LOCAL lock_timeout='250ms';
SET LOCAL statement_timeout='5s';

CREATE TABLE memory_read_object_wake_deliveries (
    hash text COLLATE "C" NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    repo_id text COLLATE "C" NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version>0),
    next_attempt timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(next_attempt)),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('','temporary_failure','invalid_source')),
    PRIMARY KEY(hash,repo_id)
);

CREATE INDEX memory_read_object_wake_deliveries_due
    ON memory_read_object_wake_deliveries(next_attempt,hash,repo_id);
