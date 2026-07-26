-- 0015: Session token at-rest hashing — the token column stores the original instead of sha256("tkh_" prefix).
-- Even if the DB is leaked, login materials do not appear. The original exists only in the issuance response/cookie.
--
-- hint — the last 8 characters of the original token (for listing and revocation identification). kind — 'web' | 'cli'.
-- Existing plaintext records are promoted to hash records at usage time by the app (lazy migration).

ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS hint TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
