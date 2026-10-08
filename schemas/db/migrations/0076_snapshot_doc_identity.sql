-- Metadata preparation only: this does not enable root document ingestion,
-- advertise an identity capability, or provide validation/publication proof.
-- The empty discriminator is the sole legacy spelling. The constant default
-- preserves existing rows without rewriting catalog history or epochs.
ALTER TABLE snapshots ADD COLUMN doc_identity text NOT NULL DEFAULT ''
    CHECK (doc_identity IN ('', 'cxt-manifest-sha256-v1'));

-- An existing snapshot ID cannot be relabeled as another document identity.
CREATE FUNCTION cxt_snapshot_doc_identity_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.doc_identity IS DISTINCT FROM OLD.doc_identity THEN
        RAISE EXCEPTION 'snapshot document identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER snapshot_doc_identity_immutable BEFORE UPDATE OF doc_identity ON snapshots
FOR EACH ROW EXECUTE FUNCTION cxt_snapshot_doc_identity_immutable();

-- Keep legacy after-images byte-for-byte equivalent. Only explicitly tagged
-- metadata adds a field; retained checkpoints and cached Merkle roots stay fixed.
CREATE OR REPLACE FUNCTION cxt_catalog_image(k text, r jsonb) RETURNS jsonb
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
        'compaction_count',COALESCE(r->'compaction_count','0'::jsonb)) || CASE WHEN COALESCE(r->>'doc_identity','') = '' THEN '{}'::jsonb
          ELSE jsonb_build_object('doc_identity',r->'doc_identity') END
    WHEN 'ref' THEN jsonb_build_object(
        'repo_id',r->'repo_id','kind',r->'kind','name',r->'name',
        'target',COALESCE(r->>'target',''),'symbolic',COALESCE(r->>'symbolic',''),
        'branch_id',COALESCE(r->>'branch_id',''))
    WHEN 'history' THEN r->'event'
    WHEN 'protocol' THEN jsonb_build_object('context_protocol',r->'context_protocol')
    END
$$;

