-- What the collectors of a repository said about themselves, and the pushes
-- they said their clones made. A collector is the program of cxt that uploads
-- sessions from one computer without being asked. With these two tables the
-- server can say of a push that came without a session whether it was made on
-- a computer where cxt runs.
--
-- One row for each computer of each member: the last thing its collector
-- said. run_error is empty when its last run ended well.
CREATE TABLE reported_collectors (
    repo_id     TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    account     TEXT NOT NULL CHECK (btrim(account) <> ''),
    machine     TEXT NOT NULL CHECK (octet_length(machine) <= 256),
    cxt_version TEXT NOT NULL CHECK (octet_length(cxt_version) <= 64),
    run_error   TEXT NOT NULL CHECK (octet_length(run_error) <= 512),
    heard_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (repo_id, account, machine)
);

-- A push a collector claimed: who claimed it first, and when the clone made
-- it by its own clock. clean becomes true once a run of that collector ended
-- well after the push, and stays true.
CREATE TABLE claimed_pushes (
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL CHECK (branch <> '' AND octet_length(branch) <= 1024),
    sha        TEXT NOT NULL CHECK (sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    account    TEXT NOT NULL CHECK (btrim(account) <> ''),
    machine    TEXT NOT NULL CHECK (octet_length(machine) <= 256),
    pushed_at  TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL,
    clean      BOOLEAN NOT NULL,
    PRIMARY KEY (repo_id, branch, sha)
);

-- The newest claimed pushes, and the newest pushes the server saw.
CREATE INDEX claimed_pushes_recent ON claimed_pushes (repo_id, pushed_at DESC);
CREATE INDEX remote_git_pushes_recent ON remote_git_pushes (repo_id, seen_at DESC);

-- The newest commit of the push the server was reading when it first stored a
-- commit: the commit came with that push. Empty for a commit that was not
-- stored while reading a push the server had just seen, as every commit of
-- the first read of a repository and every commit stored before this column.
ALTER TABLE remote_git_commits ADD COLUMN head TEXT NOT NULL DEFAULT ''
    CHECK (head = '' OR head ~ '^[0-9a-f]{40}([0-9a-f]{24})?$');
