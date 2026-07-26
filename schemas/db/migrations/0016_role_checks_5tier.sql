-- 0016: Extend role CHECK constraint to 5-tier ladder — 0002 was the constraint for owner/member 2-tier.
-- Reject viewer/puller/maintainer (discovered in rehearsal: SQLSTATE 23514).

ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_role_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
  CHECK (role IN ('viewer','puller','member','maintainer','owner'));

ALTER TABLE invites DROP CONSTRAINT IF EXISTS invites_role_check;
ALTER TABLE invites ADD CONSTRAINT invites_role_check
  CHECK (role IN ('viewer','puller','member','maintainer','owner'));
