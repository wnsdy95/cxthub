-- Repository opt-in is irreversible: old name-only clients must upgrade before
-- writing after this transition. Ref identity is separate from content hashes.
ALTER TABLE repos ADD COLUMN context_protocol integer NOT NULL DEFAULT 0 CHECK (context_protocol IN (0,1));
ALTER TABLE refs ADD COLUMN branch_id text NOT NULL DEFAULT '';
