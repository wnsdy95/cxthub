CREATE TABLE pending_projection_work (
    repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT false,
    source_epoch uuid NOT NULL DEFAULT gen_random_uuid(),
    dataset uuid NOT NULL DEFAULT gen_random_uuid(),
    applied_seq bigint NOT NULL DEFAULT -1 CHECK (applied_seq >= -1),
    input_seq bigint NOT NULL DEFAULT -1 CHECK (input_seq >= -1),
    cursor text NOT NULL DEFAULT '' CHECK (octet_length(cursor) <= 16384),
    phase text NOT NULL DEFAULT 'capture' CHECK (phase IN ('capture','project','idle','needs_attention')),
    lease_until timestamptz,
    fence bigint NOT NULL DEFAULT 0 CHECK (fence >= 0),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt timestamptz NOT NULL DEFAULT clock_timestamp(),
    reason text NOT NULL DEFAULT '' CHECK (reason IN (
        '', 'source_reset', 'lease_lost', 'resource_limit', 'invalid_source',
        'access_denied', 'identity_upgrade_required', 'temporary_failure')),
    pin_seq bigint NOT NULL DEFAULT 0 CHECK (pin_seq >= 0),
    retention_until timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (phase <> 'capture' OR input_seq = -1),
    CHECK (phase <> 'project' OR (input_seq >= 0 AND input_seq > applied_seq AND cursor = '')),
    CHECK (phase <> 'idle' OR (applied_seq >= 0 AND input_seq = applied_seq AND cursor = ''))
);

CREATE INDEX pending_projection_claim ON pending_projection_work(next_attempt, repo_id)
    WHERE enabled;

CREATE TABLE pending_projection_inputs (
    repo_id text NOT NULL REFERENCES pending_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    kind text COLLATE "C" NOT NULL CHECK (kind IN (
        'snapshot','ref','history','protocol','repository','pending','unsync','session_archive','reflog','revision')),
    entity_key text COLLATE "C" NOT NULL CHECK (octet_length(entity_key) BETWEEN 1 AND 4096),
    deleted boolean NOT NULL,
    image jsonb,
    PRIMARY KEY (repo_id, dataset, source_seq, kind, entity_key),
    CHECK ((deleted AND image IS NULL) OR (NOT deleted AND image IS NOT NULL AND jsonb_typeof(image) = 'object'))
);

CREATE INDEX pending_projection_inputs_latest ON pending_projection_inputs
    (repo_id, dataset, kind, entity_key, source_seq DESC);

CREATE TABLE pending_projection_parts (
    repo_id text NOT NULL REFERENCES pending_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    kind text COLLATE "C" NOT NULL CHECK (octet_length(kind) BETWEEN 1 AND 128),
    entity_key text COLLATE "C" NOT NULL CHECK (octet_length(entity_key) BETWEEN 1 AND 4096),
    deleted boolean NOT NULL,
    image jsonb,
    PRIMARY KEY (repo_id, dataset, source_seq, kind, entity_key),
    CHECK ((deleted AND image IS NULL) OR (NOT deleted AND image IS NOT NULL AND image <> 'null'::jsonb))
);

CREATE INDEX pending_projection_parts_latest ON pending_projection_parts
    (repo_id, dataset, kind, entity_key, source_seq DESC);

CREATE TABLE pending_projection_ready (
    repo_id text PRIMARY KEY REFERENCES pending_projection_work(repo_id) ON DELETE CASCADE,
    dataset uuid NOT NULL,
    source_epoch uuid NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    rules integer NOT NULL CHECK (rules > 0)
);

CREATE OR REPLACE FUNCTION cxt_prune_catalog(p_repo text, p_floor bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE state repository_catalog_state;
BEGIN
    SELECT * INTO STRICT state FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF p_floor<state.floor_seq OR p_floor>state.head_seq THEN RAISE EXCEPTION 'invalid catalog retention floor'; END IF;
    IF NOT EXISTS (
        SELECT 1 FROM repository_view_source_state
        WHERE repo_id=p_repo AND epoch=state.epoch AND baseline_seq<=state.head_seq
    ) THEN
        RAISE EXCEPTION 'missing or mismatched repository view source state' USING ERRCODE='23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pending_projection_work
        WHERE repo_id=p_repo AND enabled AND source_epoch=state.epoch
          AND retention_until>clock_timestamp() AND pin_seq<p_floor
    ) THEN
        RAISE EXCEPTION 'catalog retention floor exceeds pending projection pin' USING ERRCODE='23514';
    END IF;
    DELETE FROM repository_catalog_changes old
    WHERE old.repo_id=p_repo AND old.epoch=state.epoch AND old.seq<p_floor
      AND EXISTS (SELECT 1 FROM repository_catalog_changes newer
          WHERE newer.repo_id=old.repo_id AND newer.epoch=old.epoch
            AND newer.entity_kind=old.entity_kind AND newer.entity_key=old.entity_key
            AND newer.seq>old.seq AND newer.seq<=p_floor);
    DELETE FROM repository_view_source_changes old
    WHERE old.repo_id=p_repo AND old.epoch=state.epoch AND old.seq<p_floor
      AND EXISTS (SELECT 1 FROM repository_view_source_changes newer
          WHERE newer.repo_id=old.repo_id AND newer.epoch=old.epoch
            AND newer.entity_kind=old.entity_kind AND newer.entity_key=old.entity_key
            AND newer.seq>old.seq AND newer.seq<=p_floor);
    UPDATE repository_catalog_state SET floor_seq=p_floor WHERE repo_id=p_repo;
END
$$;
