-- Random selectors are not bearer tokens, token hashes or display hints.
ALTER TABLE sessions ADD COLUMN credential_id text NOT NULL
    DEFAULT ('cred_' || replace(gen_random_uuid()::text, '-', ''));
CREATE UNIQUE INDEX sessions_credential_id_idx ON sessions(credential_id);

CREATE TABLE enterprise_assurance_policies (
    enterprise_id text PRIMARY KEY REFERENCES enterprises(id),
    record jsonb NOT NULL
);
-- No credential FK: revocation removes credentials but preserves evidence and
-- audit. Application reads always recheck the current credential and membership.
CREATE TABLE enterprise_credential_assurances (
    enterprise_id text NOT NULL REFERENCES enterprises(id),
    credential_id text NOT NULL,
    user_id text NOT NULL REFERENCES users(id),
    record jsonb NOT NULL,
    PRIMARY KEY(enterprise_id, credential_id)
);
CREATE INDEX enterprise_credential_assurances_user_idx
    ON enterprise_credential_assurances(enterprise_id, user_id);
