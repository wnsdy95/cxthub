CREATE TABLE doc_tool_order_sources_v1 (
  scope text PRIMARY KEY,
  source_hash text NOT NULL REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  tree_scope text NOT NULL REFERENCES doc_tool_tree_sources_v1(scope) ON DELETE CASCADE,
  keys_scope text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  state text NOT NULL DEFAULT 'building' CHECK (state IN ('building','complete')),
  plan_count bigint CHECK (plan_count >= 0),
  UNIQUE(scope,tree_scope),
  FOREIGN KEY(keys_scope,tree_scope) REFERENCES doc_tool_key_sources_v1(scope,tree_scope) ON DELETE CASCADE,
  CHECK ((state='complete') = (plan_count IS NOT NULL))
);

CREATE INDEX doc_tool_order_sources_v1_owner ON doc_tool_order_sources_v1(source_hash);
CREATE INDEX doc_tool_order_sources_v1_tree ON doc_tool_order_sources_v1(tree_scope);
CREATE INDEX doc_tool_order_sources_v1_keys ON doc_tool_order_sources_v1(keys_scope);

CREATE TABLE doc_tool_order_plans_v1 (
  scope text NOT NULL,
  tree_scope text NOT NULL,
  parent_from bigint NOT NULL CHECK (parent_from >= 0),
  plan_hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  PRIMARY KEY(scope,parent_from),
  FOREIGN KEY(scope,tree_scope) REFERENCES doc_tool_order_sources_v1(scope,tree_scope) ON DELETE CASCADE,
  FOREIGN KEY(tree_scope,parent_from) REFERENCES doc_tool_tree_nodes_v1(scope,node_from) ON DELETE CASCADE
);

CREATE FUNCTION cxthub_guard_doc_tool_order_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_scope text;
  next_scope text;
  source_state text;
BEGIN
  IF TG_OP<>'INSERT' THEN
    previous_scope=OLD.scope;
  END IF;
  IF TG_OP<>'DELETE' THEN
    next_scope=NEW.scope;
  END IF;
  FOR source_state IN
    SELECT source.state FROM doc_tool_order_sources_v1 source
    JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope
    WHERE source.scope=previous_scope OR source.scope=next_scope
    ORDER BY source.scope FOR UPDATE OF source
  LOOP
    IF TG_OP='UPDATE' THEN
      IF NEW IS NOT DISTINCT FROM OLD THEN
        CONTINUE;
      END IF;
      RAISE EXCEPTION 'ready tool order plans are immutable' USING ERRCODE='23514';
    ELSIF TG_OP='DELETE' THEN
      RAISE EXCEPTION 'ready tool order plans are immutable' USING ERRCODE='23514';
    END IF;
    IF source_state='complete' THEN
      RAISE EXCEPTION 'completed tool order plans are immutable' USING ERRCODE='23514';
    END IF;
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_tool_order_plans_v1_complete_guard
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_order_plans_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_order_plan();

CREATE FUNCTION cxthub_guard_doc_tool_order_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN
    IF NEW.state='complete' THEN
      RAISE EXCEPTION 'tool order sources must complete after preparation' USING ERRCODE='23514';
    END IF;
  ELSIF OLD.state='complete' AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'completed tool order sources are immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_tool_order_sources_v1_complete_guard
  BEFORE INSERT OR UPDATE ON doc_tool_order_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_order_source();
