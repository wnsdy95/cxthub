-- The migration and its baseline are one transaction. Block source writers
-- until every trigger and initial image is visible. Publication revisions keep
-- their narrower graph/pending semantics.
LOCK TABLE repos, snapshots, refs, context_history IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE repository_catalog_state (
    repo_id text PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    epoch uuid NOT NULL DEFAULT gen_random_uuid(),
    head_seq bigint NOT NULL DEFAULT 0 CHECK (head_seq >= 0),
    floor_seq bigint NOT NULL DEFAULT 0 CHECK (floor_seq >= 0 AND floor_seq <= head_seq),
    writer_xid xid8
);
CREATE TABLE repository_catalog_changes (
    repo_id text NOT NULL REFERENCES repository_catalog_state(repo_id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq >= 0),
    entity_kind text COLLATE "C" NOT NULL CHECK (entity_kind IN ('snapshot','ref','history','protocol')),
    entity_key text COLLATE "C" NOT NULL,
    deleted boolean NOT NULL,
    after_image jsonb,
    PRIMARY KEY (repo_id, epoch, seq, entity_kind, entity_key),
    CHECK ((deleted AND after_image IS NULL) OR (NOT deleted AND after_image IS NOT NULL))
);
CREATE INDEX repository_catalog_latest ON repository_catalog_changes
    (repo_id, epoch, entity_kind, entity_key, seq DESC);

-- Explicit wire projection: adding a source-table column must never silently
-- expose it or change this version's meaning. Snapshot.Branches is a separate
-- graph projection, not stored snapshot metadata. Ref images are raw refs.
CREATE FUNCTION cxt_catalog_image(k text, r jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE k
    WHEN 'snapshot' THEN jsonb_build_object(
        'id',r->'id', 'repo_id',r->'repo_id', 'branch',r->'branch',
        'parents',r->'parents', 'doc_hash',r->'doc_hash',
        'memory_hash',COALESCE(r->>'memory_hash',''),
        'claude_settings',COALESCE(r->>'claude_settings',''),
        'agents_settings',COALESCE(r->>'agents_settings',''),
        'codex_settings',COALESCE(r->>'codex_settings',''),
        'provider',r->'provider', 'fidelity',r->'fidelity', 'message',r->'message',
        'author',jsonb_build_object('name',r->'author_name','email',r->'author_email','team',r->'author_team'),
        'created_at',r->'created_at', 'grafted',COALESCE(r->'grafted','false'::jsonb),
        'graft_parents',COALESCE(NULLIF(r->'graft_parents','null'::jsonb),'[]'::jsonb),
        'graft_seq',COALESCE(r->'graft_seq','0'::jsonb),
        'session_id',COALESCE(r->>'session_id',''),
        'models',COALESCE(NULLIF(r->'models','null'::jsonb),'[]'::jsonb),
        'compaction_count',COALESCE(r->'compaction_count','0'::jsonb))
    WHEN 'ref' THEN jsonb_build_object(
        'repo_id',r->'repo_id','kind',r->'kind','name',r->'name',
        'target',COALESCE(r->>'target',''),'symbolic',COALESCE(r->>'symbolic',''),
        'branch_id',COALESCE(r->>'branch_id',''))
    WHEN 'history' THEN r->'event'
    WHEN 'protocol' THEN jsonb_build_object('context_protocol',r->'context_protocol')
    END
$$;

CREATE FUNCTION cxt_catalog_key(k text, r jsonb) RETURNS text
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE WHEN k='ref' THEN jsonb_build_array(r->>'kind',r->>'name')::text ELSE r->>'id' END
$$;

CREATE FUNCTION cxt_record_catalog(p_repo text, k text, ek text, gone boolean, image jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE generation bigint; incarnation uuid;
BEGIN
    -- A transactional counter, not a sequence: a second writer cannot obtain
    -- a higher generation until this row lock is released at COMMIT/ROLLBACK.
    -- xid8 only coalesces this transaction; it is never a client cursor.
    UPDATE repository_catalog_state
       SET head_seq = head_seq+1,
           writer_xid = pg_current_xact_id()
     WHERE repo_id=p_repo AND writer_xid IS DISTINCT FROM pg_current_xact_id()
     RETURNING head_seq,epoch INTO generation,incarnation;
    IF NOT FOUND THEN
        -- A surviving mutation in this top-level transaction already holds
        -- the row lock. Reuse it without generating another heap/WAL version.
        -- A rolled-back first mutation also rolls back its writer_xid.
        SELECT head_seq,epoch INTO generation,incarnation FROM repository_catalog_state
        WHERE repo_id=p_repo AND writer_xid=pg_current_xact_id();
    END IF;
    IF NOT FOUND THEN
        -- Parent deletion cascades remove this scope entirely. Other missing
        -- states are corruption and must abort the source mutation.
        IF NOT EXISTS (SELECT 1 FROM repos WHERE id=p_repo) THEN RETURN; END IF;
        RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
    END IF;
    INSERT INTO repository_catalog_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    VALUES(p_repo,incarnation,generation,k,ek,gone,image)
    ON CONFLICT (repo_id,epoch,seq,entity_kind,entity_key)
    DO UPDATE SET deleted=EXCLUDED.deleted,after_image=EXCLUDED.after_image;
END
$$;

CREATE FUNCTION cxt_capture_catalog() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text := TG_ARGV[0]; before_row jsonb; after_row jsonb;
        before_repo text; after_repo text; before_key text; after_key text;
        before_image jsonb; after_image jsonb;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        before_row=to_jsonb(OLD);
        before_repo=CASE WHEN k='protocol' THEN before_row->>'id' ELSE before_row->>'repo_id' END;
        before_key=cxt_catalog_key(k,before_row);
        before_image=cxt_catalog_image(k,before_row);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        after_row=to_jsonb(NEW);
        after_repo=CASE WHEN k='protocol' THEN after_row->>'id' ELSE after_row->>'repo_id' END;
        after_key=cxt_catalog_key(k,after_row);
        after_image=cxt_catalog_image(k,after_row);
    END IF;
    IF k='protocol' AND TG_OP='INSERT' THEN
        INSERT INTO repository_catalog_state(repo_id) VALUES(after_repo);
    END IF;
    IF TG_OP='UPDATE' AND before_repo=after_repo AND before_key=after_key AND before_image=after_image THEN
        RETURN NULL;
    END IF;
    IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND (before_repo<>after_repo OR before_key<>after_key)) THEN
        PERFORM cxt_record_catalog(before_repo,k,before_key,true,NULL);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM cxt_record_catalog(after_repo,k,after_key,false,after_image);
    END IF;
    RETURN NULL;
END
$$;

-- Seed all existing rows without fabricating historical mutation times. This
-- function is also used only behind the maintenance reset's source locks.
CREATE FUNCTION cxt_seed_catalog(p_repo text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO repository_catalog_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    SELECT st.repo_id,st.epoch,0,src.kind,cxt_catalog_key(src.kind,src.row),false,cxt_catalog_image(src.kind,src.row)
    FROM repository_catalog_state st
    JOIN LATERAL (
        SELECT 'protocol' AS kind,to_jsonb(r) AS row FROM repos r WHERE r.id=st.repo_id
        UNION ALL SELECT 'snapshot',to_jsonb(s) FROM snapshots s WHERE s.repo_id=st.repo_id
        UNION ALL SELECT 'ref',to_jsonb(r) FROM refs r WHERE r.repo_id=st.repo_id
        UNION ALL SELECT 'history',to_jsonb(h) FROM context_history h WHERE h.repo_id=st.repo_id
    ) src ON true WHERE st.repo_id=p_repo
$$;

INSERT INTO repository_catalog_state(repo_id) SELECT id FROM repos;
SELECT cxt_seed_catalog(repo_id) FROM repository_catalog_state;

CREATE TRIGGER catalog_snapshot AFTER INSERT OR UPDATE OR DELETE ON snapshots
FOR EACH ROW EXECUTE FUNCTION cxt_capture_catalog('snapshot');
CREATE TRIGGER catalog_ref AFTER INSERT OR UPDATE OR DELETE ON refs
FOR EACH ROW EXECUTE FUNCTION cxt_capture_catalog('ref');
CREATE TRIGGER catalog_history AFTER INSERT OR UPDATE OR DELETE ON context_history
FOR EACH ROW EXECUTE FUNCTION cxt_capture_catalog('history');
CREATE TRIGGER catalog_protocol AFTER INSERT OR UPDATE ON repos
FOR EACH ROW EXECUTE FUNCTION cxt_capture_catalog('protocol');

-- TRUNCATE has no row images. Refuse it instead of silently leaving an old
-- epoch apparently current. Offline restore must rotate epochs before serving.
CREATE FUNCTION cxt_catalog_reject_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'catalog source TRUNCATE is unsupported; use transactional DELETE or offline restore with catalog epoch rotation';
END
$$;
CREATE TRIGGER catalog_snapshot_truncate BEFORE TRUNCATE ON snapshots
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_ref_truncate BEFORE TRUNCATE ON refs
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_history_truncate BEFORE TRUNCATE ON context_history
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_protocol_truncate BEFORE TRUNCATE ON repos
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();

-- Maintenance only: stop serving writers before restore/import recovery, then
-- rotate every restored repository. A restored DB cannot detect its own rewind.
CREATE FUNCTION cxt_reset_catalog(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE repos, snapshots, refs, context_history IN SHARE ROW EXCLUSIVE MODE;
    UPDATE repository_catalog_state SET epoch=gen_random_uuid(),head_seq=0,floor_seq=0,writer_xid=NULL WHERE repo_id=p_repo;
    IF NOT FOUND THEN RAISE EXCEPTION 'repository catalog not found'; END IF;
    DELETE FROM repository_catalog_changes WHERE repo_id=p_repo;
    PERFORM cxt_seed_catalog(p_repo);
END
$$;

-- Retain one anchor per key at/before the floor so a fresh paginated baseline
-- is still reconstructible. Delta cursors older than the floor must reset.
CREATE FUNCTION cxt_prune_catalog(p_repo text, p_floor bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE st repository_catalog_state;
BEGIN
    SELECT * INTO STRICT st FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF p_floor<st.floor_seq OR p_floor>st.head_seq THEN RAISE EXCEPTION 'invalid catalog retention floor'; END IF;
    DELETE FROM repository_catalog_changes old
    WHERE old.repo_id=p_repo AND old.epoch=st.epoch AND old.seq<p_floor
      AND EXISTS (SELECT 1 FROM repository_catalog_changes newer
          WHERE newer.repo_id=old.repo_id AND newer.epoch=old.epoch
            AND newer.entity_kind=old.entity_kind AND newer.entity_key=old.entity_key
            AND newer.seq>old.seq AND newer.seq<=p_floor);
    UPDATE repository_catalog_state SET floor_seq=p_floor WHERE repo_id=p_repo;
END
$$;

COMMENT ON TABLE repository_catalog_changes IS 'Version 1 synchronization metadata images, not blob availability receipts or graph publication revisions. Never truncate source tables or bypass triggers while serving clients. Restore requires epoch rotation.';
