LOCK TABLE repos, snapshots, refs, context_history, pending_contexts,
    unsync_contexts, session_archives, reflog, repository_revisions,
    repository_catalog_state, repository_catalog_changes IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE repository_view_source_state (
    repo_id text PRIMARY KEY REFERENCES repository_catalog_state(repo_id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    baseline_seq bigint NOT NULL CHECK (baseline_seq >= 0)
);

CREATE TABLE repository_view_source_changes (
    repo_id text NOT NULL REFERENCES repository_catalog_state(repo_id) ON DELETE CASCADE,
    epoch uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq >= 0),
    entity_kind text COLLATE "C" NOT NULL CHECK (entity_kind IN (
        'repository','pending','unsync','session_archive','reflog','revision')),
    entity_key text COLLATE "C" NOT NULL,
    deleted boolean NOT NULL,
    after_image jsonb,
    PRIMARY KEY (repo_id, epoch, seq, entity_kind, entity_key),
    CHECK ((deleted AND after_image IS NULL) OR (NOT deleted AND after_image IS NOT NULL))
);

CREATE INDEX repository_view_source_latest ON repository_view_source_changes
    (repo_id, epoch, entity_kind, entity_key, seq DESC);

CREATE VIEW repository_view_source_images AS
    SELECT repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image
    FROM repository_catalog_changes
    UNION ALL
    SELECT repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image
    FROM repository_view_source_changes;

CREATE FUNCTION cxt_catalog_generation(p_repo text)
RETURNS TABLE (seq bigint, epoch uuid) LANGUAGE plpgsql AS $$
BEGIN
    UPDATE repository_catalog_state AS state
       SET head_seq=state.head_seq+1,
           writer_xid=pg_current_xact_id()
     WHERE state.repo_id=p_repo AND state.writer_xid IS DISTINCT FROM pg_current_xact_id()
     RETURNING state.head_seq,state.epoch INTO seq,epoch;
    IF NOT FOUND THEN
        SELECT state.head_seq,state.epoch INTO seq,epoch
        FROM repository_catalog_state AS state
        WHERE state.repo_id=p_repo AND state.writer_xid=pg_current_xact_id();
    END IF;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM repos WHERE id=p_repo) THEN RETURN; END IF;
        RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
    END IF;
    RETURN NEXT;
END
$$;

CREATE OR REPLACE FUNCTION cxt_record_catalog(p_repo text, k text, ek text, gone boolean, image jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE generation bigint; incarnation uuid;
BEGIN
    SELECT allocated.seq,allocated.epoch INTO generation,incarnation
    FROM cxt_catalog_generation(p_repo) AS allocated;
    IF NOT FOUND THEN RETURN; END IF;
    INSERT INTO repository_catalog_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    VALUES(p_repo,incarnation,generation,k,ek,gone,image)
    ON CONFLICT (repo_id,epoch,seq,entity_kind,entity_key)
    DO UPDATE SET deleted=EXCLUDED.deleted,after_image=EXCLUDED.after_image;
END
$$;

CREATE FUNCTION cxt_repository_view_remote(raw text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE normalized text := btrim(COALESCE(raw,''),E' \t\n\r\f\013');
        components text[]; authority_parts text[]; remote_scheme text;
        authority text; hostname text; remote_path text;
BEGIN
    IF normalized COLLATE "C" !~ '^[!-~]+$'
        OR strpos(regexp_replace(normalized,'%[0-9A-Fa-f]{2}','','g'),'%')>0 THEN
        RETURN '';
    END IF;
    IF strpos(normalized,'://')>0 THEN
        components=regexp_match(normalized,
            '^([A-Za-z][A-Za-z0-9+.-]*)://([^/?#]+)(/[^?#]*)?([?][^#]*)?(#.*)?$');
        IF components IS NULL THEN RETURN ''; END IF;
        remote_scheme=lower(components[1]);
        IF remote_scheme NOT IN ('http','https','ssh','git','git+ssh') THEN RETURN ''; END IF;
        authority=components[2];
        remote_path=COALESCE(components[3],'');
        IF strpos(authority,'@')>0 THEN
            authority_parts=regexp_match(authority,'^([^@]*)@([^@]+)$');
            IF authority_parts IS NULL
                OR authority_parts[1] COLLATE "C" !~ '^[A-Za-z0-9._~!$&''()*+,;=:%-]*$' THEN
                RETURN '';
            END IF;
            authority=authority_parts[2];
        END IF;
        authority_parts=regexp_match(authority,'^([^:]+)(:([0-9]{1,5}))?$');
        IF authority_parts IS NULL THEN RETURN ''; END IF;
        hostname=authority_parts[1];
        IF authority_parts[3] IS NOT NULL AND authority_parts[3]::integer>65535 THEN RETURN ''; END IF;
    ELSE
        IF strpos(normalized,'?')>0 OR strpos(normalized,'#')>0 THEN RETURN ''; END IF;
        components=regexp_match(normalized,'^([^@]*)@([^@:]+):([^@]+)$');
        IF components IS NULL
            OR components[1] COLLATE "C" !~ '^[A-Za-z0-9._~!$&''()*+,;=:%-]*$' THEN
            RETURN '';
        END IF;
        remote_scheme='scp';
        hostname=components[2];
        remote_path=components[3];
    END IF;
    IF length(hostname)>253 OR hostname COLLATE "C" !~
        '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?([.][A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*[.]?$'
        OR EXISTS (
            SELECT 1 FROM unnest(string_to_array(rtrim(hostname,'.'),'.')) AS labels(label)
            WHERE length(label)>63
        ) THEN
        RETURN '';
    END IF;
    IF remote_path COLLATE "C" !~ '^[A-Za-z0-9._~!$&''()*+,;=:@%/-]*$' THEN RETURN ''; END IF;
    IF remote_scheme='scp' THEN RETURN 'git@'||hostname||':'||remote_path; END IF;
    RETURN remote_scheme||'://'||authority||remote_path;
END
$$;

CREATE FUNCTION cxt_repository_view_source_image(p_kind text, p_row jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE p_kind
    WHEN 'repository' THEN jsonb_build_object(
        'id',p_row->'id', 'remote_url',cxt_repository_view_remote(p_row->>'remote_url'),
        'remote_url_supported',cxt_repository_view_remote(p_row->>'remote_url')<>''
            OR btrim(COALESCE(p_row->>'remote_url',''))='',
        'default_branch',p_row->'default_branch',
        'repository_id',COALESCE(p_row->>'repository_id',''),
        'git_remote_url',cxt_repository_view_remote(p_row->>'git_remote_url'),
        'git_remote_url_supported',cxt_repository_view_remote(p_row->>'git_remote_url')<>''
            OR btrim(COALESCE(p_row->>'git_remote_url',''))='',
        'context_protocol',p_row->'context_protocol',
        'required_doc_identity',p_row->'required_doc_identity',
        'protect_default',p_row->'protect_default',
        'description',COALESCE(p_row->>'description',''),
        'website',COALESCE(p_row->>'website',''),
        'topics',COALESCE(NULLIF(NULLIF(p_row->>'topics','')::jsonb,'null'::jsonb),'[]'::jsonb))
    WHEN 'pending' THEN jsonb_build_object(
        'repo_id',p_row->'repo_id', 'session_id',p_row->'session_id',
        'branch',COALESCE(p_row#>>'{data,branch}',''),
        'provider',COALESCE(p_row#>>'{data,provider}',''),
        'target',COALESCE(p_row#>>'{data,target}',''),
        'author',jsonb_build_object(
            'name',COALESCE(p_row#>>'{data,author,name}',''),
            'email',COALESCE(p_row#>>'{data,author,email}',''),
            'team',COALESCE(p_row#>>'{data,author,team}','')),
        'updated_at',p_row#>'{data,updated_at}',
        'activity_at',p_row#>'{data,activity_at}',
        'dismissed',COALESCE(NULLIF(p_row#>'{data,dismissed}','null'::jsonb),'false'::jsonb))
    WHEN 'unsync' THEN jsonb_build_object(
        'repo_id',p_row->'repo_id', 'user',p_row->'username', 'branch',p_row->'branch',
        'target',COALESCE(p_row#>>'{data,target}',''),
        'author',jsonb_build_object(
            'name',COALESCE(p_row#>>'{data,author,name}',''),
            'email',COALESCE(p_row#>>'{data,author,email}',''),
            'team',COALESCE(p_row#>>'{data,author,team}','')),
        'updated_at',p_row#>'{data,updated_at}')
    WHEN 'session_archive' THEN jsonb_build_object(
        'repo_id',p_row->'repo_id', 'key',p_row->'key', 'snapshot_id',p_row->'snapshot_id',
        'provider',p_row->'provider', 'session_id',p_row->'session_id',
        'archived_at',p_row->'archived_at', 'archived_by',p_row->'archived_by')
    WHEN 'reflog' THEN jsonb_build_object(
        'kind',p_row->'kind', 'name',p_row->'name', 'old',p_row->'old',
        'new',p_row->'new', 'created_at',p_row->'created_at')
    WHEN 'revision' THEN jsonb_build_object(
        'graph',COALESCE(p_row->>'graph','0'),
        'pending',COALESCE(p_row->>'pending','0'),
        'evidence',COALESCE(p_row->>'evidence','0'))
    END
$$;

CREATE FUNCTION cxt_repository_view_source_key(p_kind text, p_row jsonb) RETURNS text
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE p_kind
    WHEN 'repository' THEN p_row->>'id'
    WHEN 'pending' THEN p_row->>'session_id'
    WHEN 'unsync' THEN jsonb_build_array(p_row->>'username',p_row->>'branch')::text
    WHEN 'session_archive' THEN p_row->>'key'
    WHEN 'reflog' THEN p_row->>'id'
    WHEN 'revision' THEN p_row->>'repo_id'
    END
$$;

CREATE FUNCTION cxt_record_repository_view_source(
    p_repo text, p_kind text, p_key text, p_deleted boolean, p_image jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE generation bigint; incarnation uuid;
BEGIN
    SELECT allocated.seq,allocated.epoch INTO generation,incarnation
    FROM cxt_catalog_generation(p_repo) AS allocated;
    IF NOT FOUND THEN RETURN; END IF;
    IF NOT EXISTS (
        SELECT 1 FROM repository_view_source_state
        WHERE repo_id=p_repo AND epoch=incarnation AND baseline_seq<=generation
    ) THEN
        RAISE EXCEPTION 'missing or mismatched repository view source state' USING ERRCODE='23514';
    END IF;
    INSERT INTO repository_view_source_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    VALUES(p_repo,incarnation,generation,p_kind,p_key,p_deleted,p_image)
    ON CONFLICT (repo_id,epoch,seq,entity_kind,entity_key)
    DO UPDATE SET deleted=EXCLUDED.deleted,after_image=EXCLUDED.after_image;
END
$$;

CREATE FUNCTION cxt_capture_repository_view_source() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source_kind text := TG_ARGV[0]; before_row jsonb; after_row jsonb;
        before_repo text; after_repo text; before_key text; after_key text;
        before_image jsonb; after_image jsonb;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        before_row=to_jsonb(OLD);
        before_repo=CASE WHEN source_kind='repository' THEN before_row->>'id' ELSE before_row->>'repo_id' END;
        before_key=cxt_repository_view_source_key(source_kind,before_row);
        before_image=cxt_repository_view_source_image(source_kind,before_row);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        after_row=to_jsonb(NEW);
        after_repo=CASE WHEN source_kind='repository' THEN after_row->>'id' ELSE after_row->>'repo_id' END;
        after_key=cxt_repository_view_source_key(source_kind,after_row);
        after_image=cxt_repository_view_source_image(source_kind,after_row);
    END IF;
    IF source_kind='repository' AND TG_OP='INSERT' THEN
        INSERT INTO repository_view_source_state(repo_id,epoch,baseline_seq)
        SELECT repo_id,epoch,head_seq FROM repository_catalog_state WHERE repo_id=after_repo;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' AND before_repo IS NOT DISTINCT FROM after_repo
        AND before_key IS NOT DISTINCT FROM after_key AND before_image IS NOT DISTINCT FROM after_image THEN
        RETURN NULL;
    END IF;
    IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND
        (before_repo IS DISTINCT FROM after_repo OR before_key IS DISTINCT FROM after_key)) THEN
        PERFORM cxt_record_repository_view_source(before_repo,source_kind,before_key,true,NULL);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM cxt_record_repository_view_source(after_repo,source_kind,after_key,false,after_image);
    END IF;
    IF source_kind='repository' AND TG_OP='INSERT'
        AND NOT EXISTS (SELECT 1 FROM repository_revisions WHERE repo_id=after_repo) THEN
        PERFORM cxt_record_repository_view_source(after_repo,'revision',after_repo,false,
            cxt_repository_view_source_image('revision',jsonb_build_object('repo_id',after_repo)));
    END IF;
    RETURN NULL;
END
$$;

CREATE FUNCTION cxt_seed_repository_view_source(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE state repository_catalog_state;
BEGIN
    SELECT * INTO state FROM repository_catalog_state WHERE repo_id=p_repo FOR UPDATE;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM repos WHERE id=p_repo) THEN RETURN; END IF;
        RAISE EXCEPTION 'missing repository catalog state' USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM repository_view_source_state
        WHERE repo_id=p_repo AND epoch=state.epoch AND baseline_seq=state.head_seq
    ) THEN
        RAISE EXCEPTION 'missing or mismatched repository view source baseline' USING ERRCODE='23514';
    END IF;
    INSERT INTO repository_view_source_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
    SELECT state.repo_id,state.epoch,state.head_seq,source.kind,
        cxt_repository_view_source_key(source.kind,source.row),false,
        cxt_repository_view_source_image(source.kind,source.row)
    FROM (
        SELECT 'repository' AS kind,to_jsonb(repository) AS row
        FROM repos repository WHERE repository.id=p_repo
        UNION ALL SELECT 'pending',to_jsonb(pending) FROM pending_contexts pending WHERE pending.repo_id=p_repo
        UNION ALL SELECT 'unsync',to_jsonb(unsync) FROM unsync_contexts unsync WHERE unsync.repo_id=p_repo
        UNION ALL SELECT 'session_archive',to_jsonb(archive) FROM session_archives archive WHERE archive.repo_id=p_repo
        UNION ALL SELECT 'reflog',to_jsonb(entry) FROM reflog entry WHERE entry.repo_id=p_repo
        UNION ALL SELECT 'revision',to_jsonb(revision) FROM repository_revisions revision WHERE revision.repo_id=p_repo
        UNION ALL SELECT 'revision',jsonb_build_object('repo_id',p_repo)
            WHERE NOT EXISTS (SELECT 1 FROM repository_revisions WHERE repo_id=p_repo)
    ) AS source;
END
$$;

ALTER FUNCTION cxt_seed_catalog(text) RENAME TO cxt_seed_sync_catalog;

CREATE FUNCTION cxt_seed_catalog(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM cxt_seed_sync_catalog(p_repo);
    PERFORM cxt_seed_repository_view_source(p_repo);
END
$$;

CREATE TRIGGER catalog_repository_view AFTER INSERT OR UPDATE OR DELETE ON repos
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('repository');
CREATE TRIGGER catalog_view_pending AFTER INSERT OR UPDATE OR DELETE ON pending_contexts
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('pending');
CREATE TRIGGER catalog_view_unsync AFTER INSERT OR UPDATE OR DELETE ON unsync_contexts
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('unsync');
CREATE TRIGGER catalog_view_session_archive AFTER INSERT OR UPDATE OR DELETE ON session_archives
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('session_archive');
CREATE TRIGGER catalog_view_reflog AFTER INSERT OR UPDATE OR DELETE ON reflog
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('reflog');
CREATE TRIGGER catalog_view_revision AFTER INSERT OR UPDATE OR DELETE ON repository_revisions
FOR EACH ROW EXECUTE FUNCTION cxt_capture_repository_view_source('revision');

CREATE TRIGGER catalog_view_pending_truncate BEFORE TRUNCATE ON pending_contexts
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_view_unsync_truncate BEFORE TRUNCATE ON unsync_contexts
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_view_session_archive_truncate BEFORE TRUNCATE ON session_archives
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_view_reflog_truncate BEFORE TRUNCATE ON reflog
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();
CREATE TRIGGER catalog_view_revision_truncate BEFORE TRUNCATE ON repository_revisions
FOR EACH STATEMENT EXECUTE FUNCTION cxt_catalog_reject_truncate();

CREATE OR REPLACE FUNCTION cxt_reset_catalog(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE incarnation uuid;
BEGIN
    LOCK TABLE repos, snapshots, refs, context_history, pending_contexts,
        unsync_contexts, session_archives, reflog, repository_revisions IN SHARE ROW EXCLUSIVE MODE;
    UPDATE repository_catalog_state
    SET epoch=gen_random_uuid(),head_seq=0,floor_seq=0,writer_xid=NULL WHERE repo_id=p_repo
    RETURNING epoch INTO incarnation;
    IF NOT FOUND THEN RAISE EXCEPTION 'repository catalog not found'; END IF;
    DELETE FROM repository_catalog_changes WHERE repo_id=p_repo;
    DELETE FROM repository_view_source_changes WHERE repo_id=p_repo;
    INSERT INTO repository_view_source_state(repo_id,epoch,baseline_seq)
    VALUES(p_repo,incarnation,0)
    ON CONFLICT (repo_id) DO UPDATE SET epoch=EXCLUDED.epoch,baseline_seq=EXCLUDED.baseline_seq;
    PERFORM cxt_seed_catalog(p_repo);
END
$$;

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

INSERT INTO repository_view_source_state(repo_id,epoch,baseline_seq)
SELECT repo_id,epoch,head_seq FROM repository_catalog_state;
SELECT cxt_seed_repository_view_source(repo_id) FROM repository_catalog_state;

COMMENT ON TABLE repository_view_source_state IS
    'repository-view-v1 private baseline in the shared catalog epoch. Readers must require matching epochs and sequence >= both baseline_seq and repository_catalog_state.floor_seq. Upgrade preserves public history; earlier private states are unavailable.';
COMMENT ON TABLE repository_view_source_changes IS
    'Private allowlisted repository-view-v1 inputs, not a complete graph, Git/PR evidence, effective-memory, blob integrity or authorization journal. Public snapshot/ref/history/protocol images remain solely in repository_catalog_changes. Repository images are a partial projection: origin-dependent consumers must require remote_url_supported and git_remote_url_supported; an unsupported nonempty origin is not an absent origin.';
COMMENT ON VIEW repository_view_source_images IS
    'repository-view-v1 source union, not a public synchronization catalog extension. Consumers must validate epoch, private baseline and the shared retention floor before reconstructing a fixed generation.';
COMMENT ON FUNCTION cxt_repository_view_remote(text) IS
    'Image-only conservative remote sanitization, not a replacement for domain.SanitizeRemoteURL. Accepts ASCII DNS/IPv4-style hosts, optional 0..65535 URI ports of 1..5 digits, RFC 3986 path characters and valid percent escapes. URI schemes: http/https/ssh/git/git+ssh; strips userinfo/query/fragment. SCP requires one @, a nonempty path and no query/fragment; username becomes git. Rejects to empty local/unsupported/malformed inputs, IPv6/zone IDs, Unicode, interior whitespace/control bytes, escaped or non-DNS hosts, multiple userinfo @ signs and path characters needing encoding. Only surrounding ASCII whitespace is trimmed. Origin-dependent cutover requires separate equivalence proof.';
