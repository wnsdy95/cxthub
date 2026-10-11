CREATE TABLE git_context_heads (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    origin TEXT NOT NULL CHECK (origin <> ''),
    destination_branch_id TEXT NOT NULL CHECK (destination_branch_id <> ''),
    destination_branch TEXT NOT NULL CHECK (destination_branch <> ''),
    read_version BIGINT NOT NULL CHECK (read_version > 0),
    receipt_id TEXT NOT NULL CHECK (receipt_id ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, id),
    UNIQUE (repo_id, origin, read_version, destination_branch_id),
    UNIQUE (repo_id, origin, read_version, destination_branch),
    FOREIGN KEY (repo_id, receipt_id) REFERENCES git_head_receipts(repo_id, id) ON DELETE CASCADE
);

CREATE INDEX git_context_heads_branch_reads ON git_context_heads (repo_id, origin, destination_branch_id, read_version);

CREATE TABLE git_contribution_proofs (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    admission_id TEXT NOT NULL CHECK (admission_id ~ '^[0-9a-f]{64}$'),
    context_head_id TEXT NOT NULL CHECK (context_head_id ~ '^[0-9a-f]{64}$'),
    contribution_id TEXT NOT NULL CHECK (contribution_id ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, id),
    UNIQUE (repo_id, admission_id),
    FOREIGN KEY (repo_id, admission_id) REFERENCES git_contribution_admissions(repo_id, id) ON DELETE CASCADE,
    FOREIGN KEY (repo_id, context_head_id) REFERENCES git_context_heads(repo_id, id) ON DELETE CASCADE,
    FOREIGN KEY (repo_id, contribution_id) REFERENCES git_contributions(repo_id, id) ON DELETE CASCADE
);

CREATE INDEX git_contribution_proofs_contribution ON git_contribution_proofs (repo_id, contribution_id, id);
