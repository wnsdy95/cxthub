-- 0020: Account global personal settings — Context load fidelity (full|reconstructed|memory, ''=unconfigured).
-- Stores to web account settings via PATCH /me, and is received by CLI at load/checkout consumption point.
-- Applies the priority (--mode flag > local .cxt/config load.mode > this value > full).
ALTER TABLE users ADD COLUMN IF NOT EXISTS load_mode TEXT NOT NULL DEFAULT '';
