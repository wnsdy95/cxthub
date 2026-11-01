ALTER TABLE repo_settings
    DROP CONSTRAINT repo_settings_kind_check,
    ADD CONSTRAINT repo_settings_kind_check CHECK (kind IN ('claude', 'agents', 'codex'));
