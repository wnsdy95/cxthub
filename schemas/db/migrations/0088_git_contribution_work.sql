CREATE TABLE git_contribution_work (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    admission_id TEXT NOT NULL CHECK (admission_id ~ '^[0-9a-f]{64}$'),
    version BIGINT NOT NULL CHECK (version >= 0),
    state TEXT NOT NULL CHECK (state IN ('waiting', 'retrying', 'processing', 'completed', 'attention')),
    next_attempt TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, admission_id),
    FOREIGN KEY (repo_id, admission_id) REFERENCES git_contribution_admissions(repo_id, id) ON DELETE CASCADE
);

CREATE INDEX git_contribution_work_due ON git_contribution_work (repo_id, next_attempt, admission_id)
    WHERE state IN ('waiting', 'retrying', 'processing');

INSERT INTO git_contribution_work(repo_id, admission_id, version, state, next_attempt, lease_until, payload)
SELECT admission.repo_id, admission.id, 0, initial.state, CURRENT_TIMESTAMP, TIMESTAMPTZ '0001-01-01 00:00:00+00',
    jsonb_build_object(
        'repo_id', admission.repo_id,
        'admission_id', admission.id,
        'version', '0',
        'after_read_version', admission.payload->>'after_read_version',
        'read_cursor', admission.payload->>'after_read_version',
        'read_through', '0',
        'state', initial.state,
        'attempts', 0,
        'next_attempt', CURRENT_TIMESTAMP,
        'lease_until', '0001-01-01T00:00:00Z'
    )
FROM git_contribution_admissions admission
CROSS JOIN LATERAL (
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM git_contribution_proofs proof
        WHERE proof.repo_id=admission.repo_id AND proof.admission_id=admission.id
    ) THEN 'completed' ELSE 'waiting' END AS state
) initial;
