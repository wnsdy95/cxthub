CREATE TABLE git_contribution_admissions (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    admission_key TEXT NOT NULL CHECK (admission_key ~ '^[0-9a-f]{64}$'),
    origin TEXT NOT NULL CHECK (origin <> ''),
    receipt_id TEXT NOT NULL CHECK (receipt_id ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, id),
    UNIQUE (repo_id, admission_key),
    FOREIGN KEY (repo_id, receipt_id) REFERENCES git_head_receipts(repo_id, id) ON DELETE CASCADE
);

CREATE INDEX git_contribution_admissions_origin ON git_contribution_admissions (repo_id, origin, id);
