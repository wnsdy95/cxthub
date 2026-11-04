ALTER TABLE doc_display_jobs_v1
  DROP CONSTRAINT doc_display_jobs_v1_worker_protocol_check;

UPDATE doc_display_jobs_v1
SET worker_protocol=5,version=version+1,
  state=CASE WHEN state='running' OR (state='rejected' AND reason='unsupported_display')
    THEN 'waiting' ELSE state END,
  reason=CASE WHEN state='rejected' AND reason='unsupported_display' THEN '' ELSE reason END,
  next_attempt=CASE WHEN state='rejected' AND reason='unsupported_display'
    THEN clock_timestamp() ELSE next_attempt END,
  lease_until=NULL
WHERE profile='public-event-display-v1';

ALTER TABLE doc_display_jobs_v1
  ADD CONSTRAINT doc_display_jobs_v1_worker_protocol_check
    CHECK ((profile='tool-result-string-v1' AND worker_protocol=2)
        OR (profile='public-event-display-v1' AND worker_protocol=5));

CREATE OR REPLACE FUNCTION cxthub_guard_doc_display_protocol() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ((NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2)
      OR (NEW.profile='public-event-display-v1' AND NEW.worker_protocol=5)) IS NOT TRUE THEN
    RETURN NEW;
  END IF;
  IF NEW.state='running' THEN
    IF TG_OP='UPDATE' THEN
      IF OLD.state='running' AND NEW.version=OLD.version
        AND NEW.profile=OLD.profile AND NEW.worker_protocol=OLD.worker_protocol THEN
        RETURN NEW;
      END IF;
    END IF;
    IF NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'') IN ('2','3','4','5') THEN
      RETURN NEW;
    END IF;
    IF NEW.profile='public-event-display-v1' AND NEW.worker_protocol=5
      AND COALESCE(current_setting('cxthub.doc_display_protocol',true),'')='5' THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'document display jobs require a compatible worker protocol' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TABLE doc_tool_tree_sources_v1 (
  scope text PRIMARY KEY,
  source_hash text NOT NULL REFERENCES doc_display_sources_v1(source_hash) ON DELETE CASCADE,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  root_from bigint NOT NULL CHECK (root_from >= 0),
  root_to bigint NOT NULL CHECK (root_to > root_from),
  state text NOT NULL DEFAULT 'scanning' CHECK (state IN ('scanning','complete')),
  node_count bigint CHECK (node_count > 0),
  CHECK ((state='complete') = (node_count IS NOT NULL))
);

CREATE INDEX doc_tool_tree_sources_v1_owner ON doc_tool_tree_sources_v1(source_hash);

CREATE TABLE doc_tool_tree_nodes_v1 (
  scope text NOT NULL REFERENCES doc_tool_tree_sources_v1(scope) ON DELETE CASCADE,
  node_from bigint NOT NULL CHECK (node_from >= 0),
  node_to bigint NOT NULL CHECK (node_to > node_from),
  parent_from bigint NOT NULL CHECK (parent_from >= -1),
  ordinal bigint NOT NULL CHECK (ordinal >= 0),
  key_from bigint,
  key_to bigint,
  value_type smallint NOT NULL CHECK (value_type IN (123,91,34,48,110,116,102)),
  child_count bigint NOT NULL CHECK (child_count >= 0),
  depth integer NOT NULL CHECK (depth BETWEEN 0 AND 512),
  node_hash text NOT NULL,
  bytes bytea NOT NULL CHECK (octet_length(bytes) BETWEEN 1 AND 2048),
  PRIMARY KEY (scope,node_from),
  UNIQUE(scope,parent_from,ordinal),
  CHECK ((key_from IS NULL) = (key_to IS NULL)),
  CHECK (key_from IS NULL OR (key_from >= 0 AND key_to >= key_from+2 AND key_to < node_from)),
  CHECK ((parent_from=-1 AND ordinal=0 AND depth=0 AND key_from IS NULL)
      OR (parent_from>=0 AND parent_from<node_from AND depth>0)),
  CHECK (value_type IN (123,91) OR child_count=0)
);
