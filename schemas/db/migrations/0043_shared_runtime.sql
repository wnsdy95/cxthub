CREATE TABLE device_pairings (
 code TEXT PRIMARY KEY,
 poll_hash TEXT NOT NULL,
 user_id TEXT NOT NULL DEFAULT '',
 label TEXT NOT NULL DEFAULT '',
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX device_pairings_expiry ON device_pairings(expires_at);
CREATE TABLE request_allowances (
 key TEXT PRIMARY KEY,
 available_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX request_allowances_expiry ON request_allowances(expires_at);
