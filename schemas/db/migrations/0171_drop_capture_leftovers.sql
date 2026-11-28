-- 0171: drop the tables of the capture approach that 0169 had to leave.
--
-- THIS MIGRATION DESTROYS DATA. What goes: the captures themselves
-- (snapshots, refs, context_history, pending_contexts, unsync_contexts,
-- session_archives, reflog, and the settings objects they held), the Git
-- evidence and pull request tables of captures, the change journals the
-- triggers kept for captures (catalog, view source, graph source, memory
-- source, projection work and memory read wakes), and the checkpoints of the
-- legacy document normalization. It cannot be brought back from this
-- database. Sessions are not touched. Take a backup before applying it.
--
-- No server source names these tables. 0169 had to keep them because objects
-- that stay still named them: the operator function cxt_reset_catalog, the
-- triggers on repos, blobs, blob_source_incarnations, repo_blobs,
-- repository_revisions and repository_catalog_state, the view
-- storage_source_objects with the meter_settings trigger, and the foreign key
-- legacy_checkpoint_current_root of doc_legacy_byte_stages_v1. This
-- migration first changes those objects so that what stays keeps doing
-- exactly its live work, then drops the tables, two views over them, the
-- functions that no trigger, function or server source calls any more (among
-- them those 0169 left without their tables), and four sequences:
--
-- - The catalog trigger on repos still creates the repository_catalog_state
--   row of a new repository, whose epoch fences the repository's document
--   jobs. It no longer writes a change journal.
-- - cxt_reset_catalog still rotates a repository's epoch under a lock on
--   repos, as a restore needs. It no longer reseeds journals.
-- - A new blob still gets its source incarnation. It no longer clears a
--   memory read wake.
-- - TRUNCATE stays refused on repos, repo_blobs and repository_revisions.
-- - Storage accounting releases the bytes of the settings objects through its
--   own trigger, as for any deletion; it no longer counts settings objects.
-- - The legacy byte stages lose their checkpoint roots, and the checkpoint
--   objects are deleted through their own guard and accounting triggers, so
--   that doc_legacy_byte_usage_v1 no longer counts them.
--
-- What stays although no server source names it directly:
-- storage_usage_objects (storage accounting reads it), and doc_read_events_v2
-- with doc_read_indexes_v2 (the views doc_read_index_current and
-- doc_read_event_locations_current still read them for documents indexed
-- before 0066).
--
-- Nothing is dropped with CASCADE: if anything that stays still depended on
-- one of these objects, the migration fails instead of removing it.

-- The settings objects of captures. Deleting them first lets the meter_settings
-- trigger release the storage they were counted for.
DELETE FROM settings_objects;

-- The checkpoints of the legacy document normalization. A stage's current
-- root is cleared with its guard switched off for that one statement: nothing
-- may set a root any more, and a stage whose source document changed could
-- not pass the guard. The objects are then deleted under their own protocol,
-- which releases their accounting; their staging rows, structures and edges
-- go with them. The deferred check of the edges is run at once, since a table
-- with checks still pending cannot be dropped.
ALTER TABLE doc_legacy_byte_stages_v1 DROP CONSTRAINT legacy_checkpoint_current_root;
ALTER TABLE doc_legacy_byte_stages_v1 DISABLE TRIGGER guard_legacy_byte_stage;
UPDATE doc_legacy_byte_stages_v1 SET checkpoint_hash=NULL, checkpoint_bytes=0 WHERE checkpoint_hash IS NOT NULL;
ALTER TABLE doc_legacy_byte_stages_v1 ENABLE TRIGGER guard_legacy_byte_stage;
SET LOCAL cxt.legacy_byte_checkpoint_protocol = '1';
DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1;
SET LOCAL cxt.legacy_byte_checkpoint_protocol = '';
SET CONSTRAINTS doc_legacy_checkpoint_edges_v_doc_hash_stage_id_child_hash_fkey IMMEDIATE;

-- Triggers on tables that stay whose only work was for the dropped tables.
DROP TRIGGER memory_read_wake_object ON blobs;
DROP TRIGGER memory_read_wake_incarnation ON blob_source_incarnations;
DROP TRIGGER memory_source_ownership_insert ON repo_blobs;
DROP TRIGGER memory_source_ownership_update ON repo_blobs;
DROP TRIGGER memory_source_ownership_delete ON repo_blobs;
DROP TRIGGER catalog_repository_graph ON repos;
DROP TRIGGER catalog_repository_view ON repos;
DROP TRIGGER memory_source_repository ON repos;
DROP TRIGGER memory_read_wake_catalog ON repository_catalog_state;
DROP TRIGGER catalog_view_revision ON repository_revisions;

-- What stays, without the part that wrote or read the dropped tables. The
-- trigger catalog_protocol on repos keeps its events and argument.
CREATE OR REPLACE FUNCTION cxt_capture_catalog() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' AND TG_ARGV[0]='protocol' THEN
        INSERT INTO repository_catalog_state(repo_id) VALUES(NEW.id);
    END IF;
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION cxt_initialize_blob_incarnation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO blob_source_incarnations(hash) VALUES(NEW.hash);
    RETURN NULL;
END
$$;

-- An offline restore rotates every restored repository's epoch before the
-- database serves again (see docs/CATALOG_SYNC.md).
CREATE OR REPLACE FUNCTION cxt_reset_catalog(p_repo text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE repos IN SHARE ROW EXCLUSIVE MODE;
    UPDATE repository_catalog_state
    SET epoch=gen_random_uuid(),head_seq=0,floor_seq=0,writer_xid=NULL WHERE repo_id=p_repo;
    IF NOT FOUND THEN RAISE EXCEPTION 'repository catalog not found'; END IF;
END
$$;

CREATE OR REPLACE VIEW storage_source_objects AS
 SELECT w.owner_namespace_id AS namespace_id, 'blob:'||b.hash AS object_key, max(octet_length(b.bytes))::bigint AS bytes
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash JOIN repos r ON r.id=rb.repo_id JOIN repositories w ON w.id=r.repository_id
 WHERE w.owner_namespace_id IS NOT NULL GROUP BY w.owner_namespace_id,b.hash
 UNION ALL
 SELECT w.owner_namespace_id,'secrets:'||s.repo_id,octet_length(s.data::text)::bigint
 FROM repo_secrets s JOIN repos r ON r.id=s.repo_id JOIN repositories w ON w.id=r.repository_id WHERE w.owner_namespace_id IS NOT NULL
 UNION ALL
 SELECT w.owner_namespace_id,'defaults:'||s.repo_id||':'||s.kind,octet_length(s.data::text)::bigint
 FROM repo_settings s JOIN repos r ON r.id=s.repo_id JOIN repositories w ON w.id=r.repository_id WHERE w.owner_namespace_id IS NOT NULL;

CREATE OR REPLACE FUNCTION cxt_storage_payload_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ns text; obj text; rid text;
BEGIN
 rid:=CASE WHEN TG_OP='DELETE' THEN OLD.repo_id ELSE NEW.repo_id END;
 SELECT w.owner_namespace_id INTO ns FROM repos r JOIN repositories w ON w.id=r.repository_id WHERE r.id=rid FOR SHARE OF r,w;
 IF TG_TABLE_NAME='repo_blobs' THEN obj:='blob:'||CASE WHEN TG_OP='DELETE' THEN OLD.hash ELSE NEW.hash END;
 ELSIF TG_TABLE_NAME='repo_settings' THEN obj:='defaults:'||rid||':'||CASE WHEN TG_OP='DELETE' THEN OLD.kind ELSE NEW.kind END;
 ELSE obj:='secrets:'||rid; END IF;
 PERFORM cxt_storage_refresh_key(ns,obj,'payload.'||lower(TG_OP));
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION cxt_storage_refresh_key(ns text, obj text, why text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE size bigint;
BEGIN
 IF ns IS NULL THEN RETURN; END IF;
 INSERT INTO storage_accounts(namespace_id) VALUES(ns) ON CONFLICT DO NOTHING;
 PERFORM 1 FROM storage_accounts WHERE namespace_id=ns FOR UPDATE;
 IF obj LIKE 'blob:%' THEN
 SELECT octet_length(b.bytes) INTO size FROM blobs b WHERE b.hash=substr(obj,6) AND EXISTS (SELECT 1 FROM repo_blobs rb JOIN repos r ON r.id=rb.repo_id JOIN repositories w ON w.id=r.repository_id WHERE rb.hash=b.hash AND w.owner_namespace_id=ns);
 ELSIF obj LIKE 'defaults:%' THEN
 SELECT octet_length(s.data::text) INTO size FROM repo_settings s JOIN repos r ON r.id=s.repo_id JOIN repositories w ON w.id=r.repository_id WHERE s.repo_id=substr(obj,10,71) AND s.kind=split_part(obj,':',4) AND w.owner_namespace_id=ns;
 ELSE SELECT octet_length(s.data::text) INTO size FROM repo_secrets s JOIN repos r ON r.id=s.repo_id JOIN repositories w ON w.id=r.repository_id WHERE s.repo_id=substr(obj,9) AND w.owner_namespace_id=ns;
 END IF;
 size:=coalesce(size,0);
 -- Re-encoding an already owned immutable object must not disable reads.
 PERFORM cxt_storage_apply(ns,obj,size,why,why='payload.representation');
END $$;

-- The guard of a legacy byte stage, without the lookup of the checkpoint
-- objects: none remains, so no stage may name a checkpoint root.
CREATE OR REPLACE FUNCTION cxt_guard_legacy_byte_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF COALESCE(current_setting('cxt.legacy_byte_stage_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'legacy byte staging requires protocol 1' USING ERRCODE='0A000';
  END IF;
  IF TG_OP='INSERT' THEN
    IF NEW.next_block<>0 OR NEW.checkpoint_sequence<>0 OR NEW.checkpoint_hash IS NOT NULL OR NEW.checkpoint_bytes<>0 THEN
      RAISE EXCEPTION 'legacy byte staging must start at zero' USING ERRCODE='23514';
    END IF;
    IF NEW.plan_hash IS NULL AND COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
      RAISE EXCEPTION 'legacy normalization staging requires checkpoint protocol 1' USING ERRCODE='0A000';
    END IF;
  ELSE
    IF (NEW.doc_hash,NEW.source_id,NEW.source_version) IS DISTINCT FROM (OLD.doc_hash,OLD.source_id,OLD.source_version) THEN
      RAISE EXCEPTION 'legacy byte original source is immutable' USING ERRCODE='23514';
    END IF;
    IF (NEW.plan_hash,NEW.spec,NEW.blocks) IS DISTINCT FROM (OLD.plan_hash,OLD.spec,OLD.blocks) THEN
      IF OLD.plan_hash IS NOT NULL OR NEW.plan_hash IS NULL OR NEW.next_block<>0
         OR NEW.complete IS DISTINCT FROM (NEW.blocks=0)
         OR COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1' THEN
        RAISE EXCEPTION 'legacy byte plan may be declared exactly once' USING ERRCODE='23514';
      END IF;
    ELSIF NEW.next_block<OLD.next_block OR NEW.next_block>OLD.next_block+8 THEN
      RAISE EXCEPTION 'legacy byte checkpoint changed' USING ERRCODE='23514';
    END IF;
    IF (NEW.checkpoint_sequence,NEW.checkpoint_hash,NEW.checkpoint_bytes)
        IS DISTINCT FROM (OLD.checkpoint_sequence,OLD.checkpoint_hash,OLD.checkpoint_bytes) THEN
      IF COALESCE(current_setting('cxt.legacy_byte_checkpoint_protocol',true),'')<>'1'
         OR NEW.checkpoint_sequence<>OLD.checkpoint_sequence+1
         OR (NEW.checkpoint_hash IS NOT NULL AND NEW.plan_hash IS NOT NULL) THEN
        RAISE EXCEPTION 'legacy normalization checkpoint requires its next sequence' USING ERRCODE='23514';
      END IF;
    END IF;
    IF NEW.next_block<>OLD.next_block AND
      (SELECT count(*) FROM public.doc_legacy_byte_stage_parts_v1 part
        WHERE part.doc_hash=NEW.doc_hash AND part.ordinal>=OLD.next_block AND part.ordinal<NEW.next_block)<>NEW.next_block-OLD.next_block THEN
      RAISE EXCEPTION 'legacy byte checkpoint skipped blocks' USING ERRCODE='23514';
    END IF;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.blobs source JOIN public.blob_source_incarnations incarnation ON incarnation.hash=source.hash
    WHERE source.hash=NEW.doc_hash AND source.doc_range_source_version=NEW.source_version AND incarnation.incarnation=NEW.source_id) THEN
    RAISE EXCEPTION 'legacy byte staging requires current original source' USING ERRCODE='23514';
  END IF;
  IF NEW.checkpoint_hash IS NOT NULL THEN
    RAISE EXCEPTION 'legacy normalization checkpoint root is not pinned' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP VIEW repository_graph_source_images;
DROP VIEW repository_view_source_images;

DROP TABLE
  context_history,
  doc_legacy_byte_stage_checkpoint_objects_v1,
  doc_legacy_checkpoint_edges_v1,
  doc_legacy_checkpoint_staging_v1,
  doc_legacy_checkpoint_structures_v1,
  git_commit_deltas,
  git_commit_trees,
  git_context_heads,
  git_contribution_admissions,
  git_contribution_proofs,
  git_contributions,
  git_head_receipts,
  git_ref_observations,
  git_tree_nodes,
  graph_projection_work,
  memory_read_object_wakes,
  memory_read_wakes,
  pending_contexts,
  pending_projection_work,
  pr_contributions,
  pr_history_checkpoints,
  pr_participations,
  reflog,
  refs,
  repository_catalog_changes,
  repository_graph_source_changes,
  repository_graph_source_state,
  repository_memory_source_state,
  repository_view_source_changes,
  repository_view_source_state,
  session_archives,
  settings_objects,
  snapshots,
  unsync_contexts
;

-- Functions that named the dropped tables, and functions that no trigger,
-- function or server source has called since 0169 dropped what they served.
DROP FUNCTION cxt_account_legacy_checkpoint_delete();
DROP FUNCTION cxt_account_legacy_checkpoint_insert();
DROP FUNCTION cxt_account_legacy_checkpoint_storage();
DROP FUNCTION cxt_advance_memory_source(text);
DROP FUNCTION cxt_capture_memory_metadata();
DROP FUNCTION cxt_capture_memory_ownership();
DROP FUNCTION cxt_capture_repository_graph_source();
DROP FUNCTION cxt_capture_repository_view_source();
DROP FUNCTION cxt_checkpoint_structure_edges();
DROP FUNCTION cxt_guard_checkpoint_edge();
DROP FUNCTION cxt_guard_checkpoint_staging();
DROP FUNCTION cxt_guard_checkpoint_structure();
DROP FUNCTION cxt_guard_doc_range_byte_protocol();
DROP FUNCTION cxt_guard_doc_range_worker_protocol();
DROP FUNCTION cxt_guard_legacy_checkpoint_object();
DROP FUNCTION cxt_guard_legacy_range_validation();
DROP FUNCTION cxt_initialize_memory_source();
DROP FUNCTION cxt_initialize_repository_graph_source();
DROP FUNCTION cxt_memory_capture_immutable();
DROP FUNCTION cxt_memory_read_object_wake();
DROP FUNCTION cxt_memory_read_scope_wake();
DROP FUNCTION cxt_memory_read_source_wake();
DROP FUNCTION cxt_notify_memory_read_wake(text, boolean);
DROP FUNCTION cxt_prune_catalog(text, bigint);
DROP FUNCTION cxt_prune_graph_source_catalog(text, bigint);
DROP FUNCTION cxt_prune_repository_view_catalog(text, bigint);
DROP FUNCTION cxt_record_catalog(text, text, text, boolean, jsonb);
DROP FUNCTION cxt_record_repository_graph_source(text, text, text, boolean, jsonb);
DROP FUNCTION cxt_record_repository_view_source(text, text, text, boolean, jsonb);
DROP FUNCTION cxt_reset_repository_view_catalog(text);
DROP FUNCTION cxt_seed_catalog(text);
DROP FUNCTION cxt_seed_repository_graph_source(text);
DROP FUNCTION cxt_seed_repository_view_source(text);
DROP FUNCTION cxt_seed_sync_catalog(text);
DROP FUNCTION cxt_catalog_generation(text);
DROP FUNCTION cxt_catalog_image(text, jsonb);
DROP FUNCTION cxt_catalog_key(text, jsonb);
DROP FUNCTION cxt_repository_graph_source_image(text, jsonb);
DROP FUNCTION cxt_repository_graph_source_key(text, jsonb);
DROP FUNCTION cxt_repository_view_source_image(text, jsonb);
DROP FUNCTION cxt_repository_view_source_key(text, jsonb);
DROP FUNCTION cxt_repository_view_remote(text);
DROP FUNCTION cxt_snapshot_doc_identity_immutable();
DROP FUNCTION cxthub_bind_doc_catalog_calculation(text, text, integer, text, bigint, text, uuid);
DROP FUNCTION cxthub_bind_doc_catalog_writer(text, text, integer, text, bigint);
DROP FUNCTION cxthub_guard_doc_catalog_claim();
DROP FUNCTION cxthub_guard_doc_catalog_job_identity();
DROP FUNCTION cxthub_guard_doc_display_object_coverage();
DROP FUNCTION cxthub_guard_doc_display_protocol();
DROP FUNCTION cxthub_guard_doc_tool_key_header();
DROP FUNCTION cxthub_guard_doc_tool_key_source();
DROP FUNCTION cxthub_guard_doc_tool_order_source();
DROP FUNCTION cxthub_queue_doc_display_generation_roots();
DROP FUNCTION cxthub_queue_doc_display_object_references();
DROP FUNCTION cxthub_queue_doc_display_order_source_roots();
DROP FUNCTION cxthub_queue_doc_display_tree_roots();
DROP FUNCTION git_contribution_supplement_id(text, text, text);
DROP FUNCTION guard_repository_initialization_receipt();

-- Sequences only the dropped tables and functions drew from.
DROP SEQUENCE graph_serving_fence;
DROP SEQUENCE memory_read_object_wake_fence_seq;
DROP SEQUENCE memory_read_wake_seq;
DROP SEQUENCE memory_read_work_fence_seq;
