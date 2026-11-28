-- 0169: drop the tables of the capture approach that nothing which stays depends on.
--
-- THIS MIGRATION DESTROYS DATA. What goes: the memory digests of captures
-- (memories), the branch list of captures (branches), and what was derived from
-- captures: prepared documents (display, find fields, tool values, range jobs),
-- graph and pending projections, prepared and captured memory, Git scans and
-- contribution supplements, PR promotion jobs. It cannot be brought back from
-- this database. Sessions are not touched. Apply it only after `cxt push` has run
-- on every computer whose sessions should be on the server.
--
-- What does NOT go here: the captures themselves (snapshots, refs,
-- context_history, pending_contexts, unsync_contexts, session_archives) and the
-- Git evidence tables. The operator function cxt_reset_catalog and the functions
-- it calls still name them, and triggers on living tables (repos, blobs,
-- repo_blobs) still write the catalog, view source, graph source, memory wake and
-- storage accounting tables. Removing those needs these functions rewritten.
--
-- The tables are chosen by rule, not by hand: no server source, operator script
-- or guide names the table; no foreign key of a table that stays points at it; no
-- function or view that stays names or reads it. A function stays when source, a
-- script or a guide names it, when a trigger on a table that stays calls it, or
-- when a function that stays names it.
--
-- Nothing is dropped with CASCADE: if anything that stays still depended on one
-- of these objects, the migration fails instead of removing it.

DROP VIEW IF EXISTS git_contribution_supplement_candidates;

DROP TABLE IF EXISTS
  branches,
  doc_display_catalog_admissions_v1,
  doc_display_catalog_census_v1,
  doc_display_catalog_lifecycle_v1,
  doc_display_generations_v1,
  doc_display_jobs_v1,
  doc_display_maintenance_cursor_v1,
  doc_display_maintenance_v1,
  doc_display_object_edges_v1,
  doc_display_object_marks_v1,
  doc_display_objects_v1,
  doc_display_sources_v1,
  doc_find_fields_jobs_v1,
  doc_find_fields_v1,
  doc_range_boundaries_v1,
  doc_range_jobs_v1,
  doc_read_events,
  doc_read_indexes,
  doc_search_events,
  doc_tool_key_members_v1,
  doc_tool_key_observations_v1,
  doc_tool_key_sources_v1,
  doc_tool_order_plans_v1,
  doc_tool_order_sources_v1,
  doc_tool_tree_nodes_v1,
  doc_tool_tree_sources_v1,
  git_admission_match,
  git_association_discovery,
  git_association_publications,
  git_change_jobs,
  git_contribution_memories,
  git_contribution_supplement_anchors,
  git_contribution_supplements,
  git_contribution_work,
  git_head_scans,
  git_scan_jobs,
  graph_projection_inputs,
  graph_projection_parts,
  graph_projection_ready,
  graph_projection_references,
  graph_projection_repairs,
  graph_qualification,
  graph_serving_heads,
  graph_serving_parts,
  graph_serving_repairs,
  memories,
  memory_capture_blocks,
  memory_capture_headers,
  memory_capture_parts,
  memory_read_chunks,
  memory_read_comparisons,
  memory_read_headers,
  memory_read_object_wake_deliveries,
  memory_read_qualifications,
  memory_read_work,
  memory_read_work_scopes,
  pending_projection_inputs,
  pending_projection_parts,
  pending_projection_ready,
  pr_contribution_memories,
  pr_promotion_jobs,
  repository_catalog_merkle_nodes,
  repository_catalog_merkle_roots,
  repository_initialization_anchors,
  repository_initializations,
  team_identities
;

DROP FUNCTION IF EXISTS cxt_memory_capture_graph_binding();
DROP FUNCTION IF EXISTS cxt_memory_capture_part_indexed();
DROP FUNCTION IF EXISTS cxt_memory_capture_part_sealed();
DROP FUNCTION IF EXISTS cxt_memory_read_chunk_indexed();
DROP FUNCTION IF EXISTS cxt_memory_read_chunk_sealed();
DROP FUNCTION IF EXISTS cxt_memory_read_comparison_binding();
DROP FUNCTION IF EXISTS cxt_memory_read_comparison_live();
DROP FUNCTION IF EXISTS cxt_memory_read_complete();
DROP FUNCTION IF EXISTS cxt_memory_read_qualification_binding();
DROP FUNCTION IF EXISTS cxt_memory_read_qualification_live();
DROP FUNCTION IF EXISTS cxt_memory_read_result_wake();
DROP FUNCTION IF EXISTS cxt_memory_read_work_live();
DROP FUNCTION IF EXISTS cxt_schedule_memory_read_wake(p_repo text, p_due timestamp with time zone);
DROP FUNCTION IF EXISTS cxthub_begin_doc_catalog_retirement(source text, expected_epoch uuid);
DROP FUNCTION IF EXISTS cxthub_bind_doc_catalog_calculation(repository text, document text, event_index integer, display_profile text, claim_version bigint, expected_source text, expected_generation uuid, expected_epoch uuid, expected_prior text);
DROP FUNCTION IF EXISTS cxthub_bind_doc_catalog_collector(source text, expected_epoch uuid);
DROP FUNCTION IF EXISTS cxthub_bind_doc_catalog_control(repository text, document text, event_index integer, display_profile text, claim_version bigint);
DROP FUNCTION IF EXISTS cxthub_doc_catalog_clear(source text, expected_epoch uuid, current_generation uuid);
DROP FUNCTION IF EXISTS cxthub_doc_catalog_empty(source text);
DROP FUNCTION IF EXISTS cxthub_finish_doc_catalog_retirement(source text, expected_epoch uuid);
DROP FUNCTION IF EXISTS cxthub_guard_doc_catalog_admission_order();
DROP FUNCTION IF EXISTS cxthub_guard_doc_catalog_job_write();
DROP FUNCTION IF EXISTS cxthub_guard_doc_catalog_lifecycle();
DROP FUNCTION IF EXISTS cxthub_guard_doc_display_object_edge();
DROP FUNCTION IF EXISTS cxthub_guard_doc_tool_catalog_delete();
DROP FUNCTION IF EXISTS cxthub_guard_doc_tool_catalog_write();
DROP FUNCTION IF EXISTS cxthub_guard_doc_tool_key_row();
DROP FUNCTION IF EXISTS cxthub_guard_doc_tool_order_plan();
DROP FUNCTION IF EXISTS cxthub_lock_doc_catalog_affinities(native_events text[], native_profiles text[], source_hashes text[]);
DROP FUNCTION IF EXISTS cxthub_lock_doc_catalog_retirement(source text, expected_epoch uuid);
DROP FUNCTION IF EXISTS cxthub_queue_doc_display_job_roots();
DROP FUNCTION IF EXISTS cxthub_queue_doc_display_maintenance(source text);
DROP FUNCTION IF EXISTS cxthub_queue_doc_display_order_plan_roots();
DROP FUNCTION IF EXISTS cxthub_record_doc_catalog_job_admission();
DROP FUNCTION IF EXISTS cxthub_record_doc_catalog_source();
DROP FUNCTION IF EXISTS cxthub_require_doc_catalog_collector(source text);
DROP FUNCTION IF EXISTS cxthub_require_doc_catalog_writer(source text, allow_unbound boolean, expected_job jsonb);
DROP FUNCTION IF EXISTS cxthub_restart_doc_catalog(source text, expected_epoch uuid, initial_hash text, initial_raw bytea);
DROP FUNCTION IF EXISTS cxthub_seed_doc_catalog_lifecycle();
DROP FUNCTION IF EXISTS cxthub_verify_doc_catalog_restart();
DROP FUNCTION IF EXISTS cxthub_wake_doc_catalog_census();
DROP FUNCTION IF EXISTS prune_doc_search_events();
