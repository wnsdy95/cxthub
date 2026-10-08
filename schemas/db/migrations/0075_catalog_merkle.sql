-- Optional derived metadata cache. No source rows, epochs, journal retention,
-- graph revisions, or blob availability receipts are changed by this migration.
CREATE TABLE repository_catalog_merkle_nodes (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    hash text NOT NULL CHECK (hash ~ '^sha256:[0-9a-f]{64}$'),
    payload bytea NOT NULL CHECK (octet_length(payload) > 0),
    PRIMARY KEY (repo_id, hash)
);
CREATE TABLE repository_catalog_merkle_roots (
    repo_id text NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq >= 0),
    root_hash text NOT NULL,
    PRIMARY KEY (repo_id, epoch, seq),
    FOREIGN KEY (repo_id, root_hash)
        REFERENCES repository_catalog_merkle_nodes(repo_id, hash)
);
COMMENT ON TABLE repository_catalog_merkle_nodes IS
    'Immutable canonical Merkle v1 node payloads. Conflicting bytes must fail; never overwrite. Derived metadata only, not document integrity or authorization.';
COMMENT ON TABLE repository_catalog_merkle_roots IS
    'Complete fixed catalog checkpoints. Old epochs remain retained but cannot be served. No automatic source or derived-cache GC.';
