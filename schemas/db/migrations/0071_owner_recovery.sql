CREATE TABLE enterprise_owner_recovery (
 enterprise_id text NOT NULL REFERENCES enterprises(id),
 user_id text NOT NULL REFERENCES users(id),
 record jsonb NOT NULL,
 PRIMARY KEY(enterprise_id,user_id)
);
-- Session deletion and role changes invalidate these records at assessment;
-- retaining the record is not retaining authority.
CREATE TABLE enterprise_owner_repair_sessions (
 enterprise_id text NOT NULL REFERENCES enterprises(id),
 session_hash text NOT NULL,
 record jsonb NOT NULL,
 PRIMARY KEY(enterprise_id,session_hash)
);
