-- Invitation and email payload commit together in WithinIdentity.
CREATE TABLE IF NOT EXISTS invitation_emails (
 invitation_id text PRIMARY KEY REFERENCES collaboration_invites(id),
 state text NOT NULL,
 next_attempt timestamptz NOT NULL,
 record jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS invitation_emails_due ON invitation_emails(next_attempt, invitation_id)
 WHERE state IN ('queued','retrying','sending');
