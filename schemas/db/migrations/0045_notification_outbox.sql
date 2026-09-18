-- Delivery happens outside the transaction. Enqueue is part of the mutation.
CREATE TABLE notification_outbox (
 id TEXT PRIMARY KEY,
 workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 destination TEXT NOT NULL,
 payload JSONB NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('pending','running','retrying','delivered','attention')),
 version BIGINT NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL,
 next_attempt TIMESTAMPTZ NOT NULL,
 lease_until TIMESTAMPTZ NOT NULL
);
CREATE INDEX notification_outbox_due ON notification_outbox(next_attempt, created_at)
 WHERE state IN ('pending','retrying','running');
CREATE INDEX notification_outbox_workspace ON notification_outbox(workspace_id, created_at DESC, id DESC);
