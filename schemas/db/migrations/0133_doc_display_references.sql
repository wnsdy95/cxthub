ALTER TABLE doc_display_sources_v1
  ADD COLUMN reference_revision bigint NOT NULL DEFAULT 0 CHECK (reference_revision >= 0);

ALTER TABLE doc_display_objects_v1
  ADD COLUMN references_version smallint NOT NULL DEFAULT 0 CHECK (references_version IN (0,1));

CREATE INDEX doc_display_objects_v1_unindexed ON doc_display_objects_v1(source_hash,hash)
  WHERE references_version=0;
CREATE INDEX doc_display_jobs_v1_source_roots ON doc_display_jobs_v1(source_hash,repo_id,doc_hash,event_index,profile);
CREATE INDEX doc_tool_tree_sources_v1_source_scope ON doc_tool_tree_sources_v1(source_hash,scope);
CREATE INDEX doc_tool_order_sources_v1_source_scope ON doc_tool_order_sources_v1(source_hash,scope);

CREATE TABLE doc_display_object_edges_v1 (
  source_hash text NOT NULL,
  parent_hash text NOT NULL,
  child_hash text NOT NULL,
  PRIMARY KEY (source_hash,parent_hash,child_hash),
  FOREIGN KEY (source_hash,parent_hash) REFERENCES doc_display_objects_v1(source_hash,hash) ON DELETE CASCADE
);

CREATE TABLE doc_display_maintenance_v1 (
  source_hash text PRIMARY KEY REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  phase text NOT NULL DEFAULT 'index' CHECK (phase IN ('index','roots_jobs','roots_generation','roots_trees','roots_plans','mark','sweep','idle')),
  epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
  source_revision bigint NOT NULL DEFAULT 0 CHECK (source_revision >= 0),
  index_after text NOT NULL DEFAULT '',
  jobs_repo text NOT NULL DEFAULT '',
  jobs_doc text NOT NULL DEFAULT '',
  jobs_event bigint NOT NULL DEFAULT -1 CHECK (jobs_event >= -1),
  jobs_profile text NOT NULL DEFAULT '',
  plans_scope text NOT NULL DEFAULT '',
  plans_parent bigint NOT NULL DEFAULT -1 CHECK (plans_parent >= -1),
  sweep_after text NOT NULL DEFAULT '',
  due_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX doc_display_maintenance_v1_due ON doc_display_maintenance_v1(due_at,source_hash)
  WHERE phase<>'idle';

CREATE TABLE doc_display_object_marks_v1 (
  source_hash text NOT NULL,
  hash text NOT NULL,
  epoch bigint NOT NULL CHECK (epoch >= 0),
  expanded boolean NOT NULL DEFAULT false,
  PRIMARY KEY (source_hash,hash),
  FOREIGN KEY (source_hash,hash) REFERENCES doc_display_objects_v1(source_hash,hash) ON DELETE CASCADE
);

CREATE INDEX doc_display_object_marks_v1_pending ON doc_display_object_marks_v1(source_hash,epoch,expanded,hash);

CREATE FUNCTION cxthub_guard_doc_display_object_coverage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.references_version=1 AND NEW.references_version<>1 THEN
    RAISE EXCEPTION 'document display reference coverage is immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_display_objects_v1_coverage_guard
  BEFORE UPDATE OF references_version ON doc_display_objects_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_display_object_coverage();

CREATE FUNCTION cxthub_guard_doc_display_object_edge() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  previous_parent text;
  next_source text;
  next_parent text;
  parent record;
BEGIN
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
    previous_parent=OLD.parent_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
    next_parent=NEW.parent_hash;
  END IF;
  FOR parent IN
    SELECT source_hash,hash,references_version FROM doc_display_objects_v1
    WHERE (source_hash=previous_source AND hash=previous_parent)
      OR (source_hash=next_source AND hash=next_parent)
    ORDER BY source_hash,hash FOR UPDATE
  LOOP
    IF parent.references_version=1 THEN
      IF TG_OP='INSERT' THEN
        IF EXISTS(SELECT 1 FROM doc_display_object_edges_v1
          WHERE source_hash=NEW.source_hash AND parent_hash=NEW.parent_hash AND child_hash=NEW.child_hash) THEN
          CONTINUE;
        END IF;
      END IF;
      RAISE EXCEPTION 'indexed document display edges are immutable' USING ERRCODE='23514';
    END IF;
  END LOOP;
  IF TG_OP='DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_display_object_edges_v1_coverage_guard
  BEFORE INSERT OR UPDATE OR DELETE ON doc_display_object_edges_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_display_object_edge();

CREATE FUNCTION cxthub_queue_doc_display_maintenance(source text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  UPDATE doc_display_sources_v1 SET reference_revision=reference_revision+1 WHERE source_hash=$1;
  IF NOT FOUND THEN
    RETURN;
  END IF;
  INSERT INTO doc_display_maintenance_v1(source_hash) VALUES($1)
  ON CONFLICT(source_hash) DO UPDATE
  SET phase=CASE WHEN doc_display_maintenance_v1.phase='idle' THEN 'index' ELSE doc_display_maintenance_v1.phase END,
    due_at=LEAST(doc_display_maintenance_v1.due_at,clock_timestamp());
END;
$$;

COMMENT ON FUNCTION cxthub_queue_doc_display_maintenance(text) IS
  'Advance only the non-key source revision before locking maintenance. Collectors must lock sources FOR NO KEY UPDATE before maintenance and must not lock jobs. FOR UPDATE conflicts with source foreign-key KEY SHARE locks held by root writers and can deadlock their revision updates.';

CREATE FUNCTION cxthub_queue_doc_display_object_references() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.hash,NEW.source_hash,NEW.bytes) IS DISTINCT FROM (OLD.hash,OLD.source_hash,OLD.bytes) THEN
      RAISE EXCEPTION 'document display objects are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
  END IF;
  PERFORM cxthub_queue_doc_display_maintenance(NEW.source_hash);
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_display_objects_v1_queue_references
  BEFORE INSERT OR UPDATE OF hash,source_hash,bytes ON doc_display_objects_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_object_references();

COMMENT ON TRIGGER doc_display_objects_v1_queue_references ON doc_display_objects_v1 IS
  'Reject object identity or byte changes while allowing exact replay updates. Fence inserts before foreign-key checks and before INSERT ON CONFLICT can reuse an existing object. Replay and coverage-only updates and collector deletes do not revise or queue the source.';

CREATE FUNCTION cxthub_guard_doc_tool_key_header() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.scope,NEW.tree_scope,NEW.bytes) IS DISTINCT FROM (OLD.scope,OLD.tree_scope,OLD.bytes) THEN
    RAISE EXCEPTION 'tool key source headers are immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_tool_key_sources_v1_header_guard
  BEFORE UPDATE OF scope,tree_scope,bytes ON doc_tool_key_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_key_header();

CREATE FUNCTION cxthub_queue_doc_display_job_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  next_source text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.profile,NEW.source_hash,NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation)
      IS NOT DISTINCT FROM (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.profile,OLD.source_hash,OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
  END IF;
  FOR queued_source IN
    SELECT DISTINCT roots.source_hash FROM unnest(ARRAY[previous_source,next_source]) AS roots(source_hash)
    WHERE roots.source_hash IS NOT NULL ORDER BY roots.source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_display_jobs_v1_queue_roots
  AFTER INSERT OR UPDATE OF repo_id,doc_hash,event_index,profile,source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation OR DELETE ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_job_roots();

CREATE FUNCTION cxthub_queue_doc_display_generation_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  next_source text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.source_hash,NEW.generation_hash,NEW.bytes)
      IS NOT DISTINCT FROM (OLD.source_hash,OLD.generation_hash,OLD.bytes) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
  END IF;
  FOR queued_source IN
    SELECT DISTINCT roots.source_hash FROM unnest(ARRAY[previous_source,next_source]) AS roots(source_hash)
    WHERE roots.source_hash IS NOT NULL ORDER BY roots.source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_display_generations_v1_queue_roots
  AFTER INSERT OR UPDATE OR DELETE ON doc_display_generations_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_generation_roots();

CREATE FUNCTION cxthub_queue_doc_display_tree_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  next_source text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.source_hash,NEW.scope,NEW.bytes) IS NOT DISTINCT FROM (OLD.source_hash,OLD.scope,OLD.bytes) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
  END IF;
  FOR queued_source IN
    SELECT DISTINCT roots.source_hash FROM unnest(ARRAY[previous_source,next_source]) AS roots(source_hash)
    WHERE roots.source_hash IS NOT NULL ORDER BY roots.source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_tool_tree_sources_v1_queue_roots
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_tree_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_tree_roots();

CREATE FUNCTION cxthub_queue_doc_display_order_source_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_source text;
  next_source text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF (NEW.source_hash,NEW.scope,NEW.bytes,NEW.tree_scope,NEW.keys_scope)
      IS NOT DISTINCT FROM (OLD.source_hash,OLD.scope,OLD.bytes,OLD.tree_scope,OLD.keys_scope) THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_source=OLD.source_hash;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_source=NEW.source_hash;
  END IF;
  FOR queued_source IN
    SELECT DISTINCT roots.source_hash FROM unnest(ARRAY[previous_source,next_source]) AS roots(source_hash)
    WHERE roots.source_hash IS NOT NULL ORDER BY roots.source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_tool_order_sources_v1_queue_roots
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_order_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_order_source_roots();

CREATE FUNCTION cxthub_queue_doc_display_order_plan_roots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_scope text;
  next_scope text;
  queued_source text;
BEGIN
  IF TG_OP='UPDATE' THEN
    IF NEW IS NOT DISTINCT FROM OLD THEN
      RETURN NULL;
    END IF;
  END IF;
  IF TG_OP<>'INSERT' THEN
    previous_scope=OLD.scope;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_scope=NEW.scope;
  END IF;
  FOR queued_source IN
    SELECT DISTINCT source_hash FROM doc_tool_order_sources_v1
    WHERE scope=previous_scope OR scope=next_scope ORDER BY source_hash
  LOOP
    PERFORM cxthub_queue_doc_display_maintenance(queued_source);
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_tool_order_plans_v1_queue_roots
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_order_plans_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_queue_doc_display_order_plan_roots();

INSERT INTO doc_display_maintenance_v1(source_hash) SELECT source_hash FROM doc_display_sources_v1;
