-- What the server itself read from a repository's Git hosting provider:
-- commits, branch heads and pull requests, and the state of that reading.
-- Nothing in these tables comes from a client. They are a cache of one
-- origin: when the repository's origin changes, the rows are removed and the
-- reading starts over.
CREATE TABLE remote_git_state (
    repo_id       TEXT PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    body          JSONB NOT NULL,
    next_at       TIMESTAMPTZ NOT NULL,
    claimed_until TIMESTAMPTZ
);

-- A stored commit is never changed.
CREATE TABLE remote_git_commits (
    repo_id      TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    sha          TEXT NOT NULL CHECK (sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    parents      TEXT[] NOT NULL CHECK (
        cardinality(parents) <= 256
        AND array_position(parents, NULL) IS NULL
        AND array_to_string(parents, ',') ~ '^([0-9a-f]{40}([0-9a-f]{24})?(,[0-9a-f]{40}([0-9a-f]{24})?)*)?$'
    ),
    author_name  TEXT NOT NULL CHECK (octet_length(author_name) <= 1024),
    author_email TEXT NOT NULL CHECK (octet_length(author_email) <= 1024),
    authored_at  TIMESTAMPTZ NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL,
    subject      TEXT NOT NULL CHECK (octet_length(subject) <= 1024),
    PRIMARY KEY (repo_id, sha)
);

-- The commit list, newest first.
CREATE INDEX remote_git_commits_listing ON remote_git_commits (repo_id, committed_at DESC, sha DESC);

-- A rebase or a cherry-pick keeps the author time, the author email and the
-- subject of a commit: copies are found by them.
CREATE INDEX remote_git_commits_copy ON remote_git_commits (repo_id, authored_at, author_email);

CREATE TABLE remote_git_branches (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name    TEXT NOT NULL CHECK (name <> '' AND octet_length(name) <= 1024),
    sha     TEXT NOT NULL CHECK (sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    PRIMARY KEY (repo_id, name)
);

CREATE TABLE remote_git_pulls (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    number  INTEGER NOT NULL CHECK (number >= 1),
    body    JSONB NOT NULL,
    PRIMARY KEY (repo_id, number)
);

-- The commits of a pull request as the provider lists them, oldest first. The
-- provider's list stops at 250 commits.
CREATE TABLE remote_git_pull_commits (
    repo_id  TEXT NOT NULL,
    number   INTEGER NOT NULL,
    position INTEGER NOT NULL CHECK (position >= 0 AND position < 250),
    sha      TEXT NOT NULL CHECK (sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    PRIMARY KEY (repo_id, number, position),
    FOREIGN KEY (repo_id, number) REFERENCES remote_git_pulls(repo_id, number) ON DELETE CASCADE
);

-- From a commit to the pull requests that list it.
CREATE INDEX remote_git_pull_commits_commit ON remote_git_pull_commits (repo_id, sha);
