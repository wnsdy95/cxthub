CREATE TABLE doc_tool_key_sources_v1 (
  scope text PRIMARY KEY,
  tree_scope text NOT NULL REFERENCES doc_tool_tree_sources_v1(scope) ON DELETE CASCADE,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  state text NOT NULL DEFAULT 'scanning' CHECK (state IN ('scanning','complete')),
  member_count bigint CHECK (member_count >= 0),
  UNIQUE(scope,tree_scope),
  CHECK ((state='complete') = (member_count IS NOT NULL))
);

CREATE INDEX doc_tool_key_sources_v1_tree ON doc_tool_key_sources_v1(tree_scope);

CREATE TABLE doc_tool_key_observations_v1 (
  scope text NOT NULL,
  tree_scope text NOT NULL,
  node_from bigint NOT NULL CHECK (node_from >= 0),
  parent_from bigint NOT NULL CHECK (parent_from >= 0 AND parent_from < node_from),
  digest text NOT NULL,
  is_array_index boolean NOT NULL,
  array_index bigint NOT NULL CHECK (array_index BETWEEN 0 AND 4294967294),
  node_hash text NOT NULL,
  PRIMARY KEY(scope,node_from),
  FOREIGN KEY(scope,tree_scope) REFERENCES doc_tool_key_sources_v1(scope,tree_scope) ON DELETE CASCADE,
  FOREIGN KEY(tree_scope,node_from) REFERENCES doc_tool_tree_nodes_v1(scope,node_from) ON DELETE CASCADE,
  CHECK (is_array_index OR array_index=0)
);

CREATE INDEX doc_tool_key_observations_v1_groups ON doc_tool_key_observations_v1(scope,parent_from,digest,node_from);

CREATE TABLE doc_tool_key_members_v1 (
  scope text NOT NULL REFERENCES doc_tool_key_sources_v1(scope) ON DELETE CASCADE,
  parent_from bigint NOT NULL CHECK (parent_from >= 0),
  digest text NOT NULL,
  first_from bigint NOT NULL CHECK (first_from > parent_from),
  last_from bigint NOT NULL CHECK (last_from >= first_from),
  is_array_index boolean NOT NULL,
  array_index bigint NOT NULL CHECK (array_index BETWEEN 0 AND 4294967294),
  PRIMARY KEY(scope,parent_from,digest),
  FOREIGN KEY(scope,first_from) REFERENCES doc_tool_key_observations_v1(scope,node_from) ON DELETE CASCADE,
  FOREIGN KEY(scope,last_from) REFERENCES doc_tool_key_observations_v1(scope,node_from) ON DELETE CASCADE,
  CHECK (is_array_index OR array_index=0)
);

CREATE INDEX doc_tool_key_members_v1_order ON doc_tool_key_members_v1(scope,parent_from,(NOT is_array_index),array_index,first_from);

ALTER TABLE doc_tool_key_observations_v1
  ADD CONSTRAINT doc_tool_key_observations_v1_member
    FOREIGN KEY(scope,parent_from,digest) REFERENCES doc_tool_key_members_v1(scope,parent_from,digest)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION cxthub_guard_doc_tool_key_row() RETURNS trigger LANGUAGE plpgsql AS $$
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
    SELECT source.state FROM doc_tool_key_sources_v1 source
    JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope
    WHERE source.scope=previous_scope OR source.scope=next_scope
    ORDER BY source.scope FOR UPDATE OF source
  LOOP
    IF source_state='complete' THEN
      IF TG_OP='UPDATE' THEN
        IF NEW IS NOT DISTINCT FROM OLD THEN
          CONTINUE;
        END IF;
      END IF;
      RAISE EXCEPTION 'completed tool key rows are immutable' USING ERRCODE='23514';
    END IF;
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE TRIGGER doc_tool_key_members_v1_complete_guard
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_key_members_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_key_row();

CREATE TRIGGER doc_tool_key_observations_v1_complete_guard
  AFTER INSERT OR UPDATE OR DELETE ON doc_tool_key_observations_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_key_row();

CREATE FUNCTION cxthub_guard_doc_tool_key_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN
    IF NEW.state='complete' THEN
      RAISE EXCEPTION 'tool key sources must complete after aggregation' USING ERRCODE='23514';
    END IF;
  ELSIF OLD.state='complete' AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'completed tool key sources are immutable' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_tool_key_sources_v1_complete_guard
  BEFORE INSERT OR UPDATE ON doc_tool_key_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_key_source();
