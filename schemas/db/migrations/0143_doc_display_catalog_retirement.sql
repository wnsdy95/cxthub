SET LOCAL lock_timeout='250ms';

LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1,doc_display_catalog_admissions_v1,
  doc_display_catalog_lifecycle_v1,doc_display_maintenance_v1,doc_display_catalog_census_v1,
  doc_tool_tree_sources_v1,doc_tool_tree_nodes_v1,doc_tool_key_sources_v1,
  doc_tool_key_observations_v1,doc_tool_key_members_v1,
  doc_tool_order_sources_v1,doc_tool_order_plans_v1 IN EXCLUSIVE MODE NOWAIT;

ALTER TABLE doc_display_catalog_lifecycle_v1
  ADD COLUMN retire_generation uuid,
  ADD COLUMN retire_phase text NOT NULL DEFAULT '' CHECK(retire_phase IN ('','orders','keys','trees','empty')),
  ADD COLUMN retire_scope text NOT NULL DEFAULT '' CHECK(octet_length(retire_scope)<=72),
  ADD CONSTRAINT doc_display_catalog_lifecycle_v1_retirement CHECK(
    (state='live' AND retire_generation IS NULL AND retire_phase='' AND retire_scope='') OR
    (state='retiring' AND retire_generation IS NOT NULL AND retire_phase<>'' AND
      retire_generation<>'00000000-0000-0000-0000-000000000000'::uuid) OR
    (state='retired' AND retire_generation IS NOT NULL AND retire_phase='empty' AND retire_scope='' AND
      retire_generation<>'00000000-0000-0000-0000-000000000000'::uuid));

CREATE INDEX doc_tool_key_members_v1_first ON doc_tool_key_members_v1(scope,first_from,parent_from,digest);
CREATE INDEX doc_tool_key_members_v1_last ON doc_tool_key_members_v1(scope,last_from,parent_from,digest);
CREATE INDEX doc_tool_key_observations_v1_tree_node ON doc_tool_key_observations_v1(tree_scope,node_from);
CREATE INDEX doc_tool_order_plans_v1_tree_parent ON doc_tool_order_plans_v1(tree_scope,parent_from);
CREATE INDEX doc_tool_key_sources_v1_tree_scope ON doc_tool_key_sources_v1(tree_scope,scope);

CREATE FUNCTION cxthub_doc_catalog_empty(source text) RETURNS boolean LANGUAGE sql AS $$
 SELECT NOT EXISTS(SELECT 1 FROM doc_tool_order_sources_v1 WHERE source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_order_plans_v1 plans JOIN doc_tool_order_sources_v1 orders USING(scope) WHERE orders.source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_key_sources_v1 keys JOIN doc_tool_tree_sources_v1 trees ON trees.scope=keys.tree_scope WHERE trees.source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_key_members_v1 members JOIN doc_tool_key_sources_v1 keys USING(scope)
   JOIN doc_tool_tree_sources_v1 trees ON trees.scope=keys.tree_scope WHERE trees.source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 observations JOIN doc_tool_tree_sources_v1 trees ON trees.scope=observations.tree_scope WHERE trees.source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_tree_nodes_v1 nodes JOIN doc_tool_tree_sources_v1 trees USING(scope) WHERE trees.source_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_tree_sources_v1 WHERE source_hash=$1);
$$;

CREATE FUNCTION cxthub_lock_doc_catalog_retirement(source text,expected_epoch uuid) RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE
  native_event text;
  native_profile text;
  locked_event text;
  locked_profile text;
  current_epoch uuid;
  current_state text;
  retired_generation uuid;
  current_generation uuid;
BEGIN
  IF source IS NULL OR source !~ '^sha256:[0-9a-f]{64}$' OR expected_epoch IS NULL
    OR expected_epoch='00000000-0000-0000-0000-000000000000'::uuid THEN
    RAISE EXCEPTION 'document catalog collector binding is invalid' USING ERRCODE='0A000';
  END IF;
  SELECT event_hash,profile INTO native_event,native_profile FROM doc_display_sources_v1 WHERE source_hash=source;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog source incarnation changed' USING ERRCODE='55000';
  END IF;
  PERFORM * FROM cxthub_lock_doc_catalog_affinities(ARRAY[native_event],ARRAY[native_profile],ARRAY[]::text[]);
  IF NOT pg_try_advisory_xact_lock(hashtextextended('cxthub-doc-catalog-use:'||native_event||':'||native_profile,0)) THEN
    RAISE EXCEPTION 'document catalog is in use' USING ERRCODE='55P03';
  END IF;
  SELECT event_hash,profile INTO locked_event,locked_profile FROM doc_display_sources_v1
    WHERE source_hash=source FOR NO KEY UPDATE NOWAIT;
  IF NOT FOUND OR (locked_event,locked_profile) IS DISTINCT FROM (native_event,native_profile) THEN
    RAISE EXCEPTION 'document catalog source incarnation changed' USING ERRCODE='55000';
  END IF;
  SELECT catalog_epoch,state,retire_generation INTO current_epoch,current_state,retired_generation
    FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=source FOR UPDATE NOWAIT;
  IF current_epoch IS DISTINCT FROM expected_epoch THEN
    RAISE EXCEPTION 'document catalog epoch changed' USING ERRCODE='55000';
  END IF;
  PERFORM source_hash FROM doc_display_maintenance_v1 WHERE source_hash=source FOR UPDATE NOWAIT;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog collector binding is invalid' USING ERRCODE='0A000';
  END IF;
  SELECT generation INTO current_generation FROM doc_display_catalog_admissions_v1
    WHERE event_hash=native_event AND profile=native_profile AND source_hash=source;
  IF current_generation IS NULL OR current_generation='00000000-0000-0000-0000-000000000000'::uuid
    OR (current_state<>'live' AND current_generation IS DISTINCT FROM retired_generation) THEN
    RAISE EXCEPTION 'document catalog source incarnation changed' USING ERRCODE='55000';
  END IF;
  RETURN current_generation;
END;
$$;

CREATE FUNCTION cxthub_doc_catalog_clear(source text,expected_epoch uuid,current_generation uuid) RETURNS boolean LANGUAGE sql AS $$
 SELECT EXISTS(SELECT 1 FROM doc_display_catalog_census_v1 census
 JOIN doc_display_sources_v1 owner ON owner.source_hash=census.source_hash
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=owner.event_hash AND admission.profile=owner.profile
   AND admission.source_hash=owner.source_hash
 WHERE census.source_hash=$1 AND census.catalog_epoch=$2 AND census.phase='clear' AND census.reason=''
   AND census.generation=$3 AND admission.generation=$3
   AND census.admission_revision=admission.admission_revision AND census.source_revision=owner.reference_revision);
$$;

CREATE FUNCTION cxthub_bind_doc_catalog_collector(source text,expected_epoch uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  generation uuid;
  current_state text;
BEGIN
  generation=cxthub_lock_doc_catalog_retirement(source,expected_epoch);
  SELECT state INTO current_state FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=source;
  IF current_state<>'retiring' THEN
    RAISE EXCEPTION 'document catalog is not retiring' USING ERRCODE='23514';
  END IF;
  PERFORM set_config('cxthub.doc_catalog_collector',jsonb_build_object('source',source,'epoch',expected_epoch,'generation',generation)::text,true);
END;
$$;

CREATE FUNCTION cxthub_require_doc_catalog_collector(source text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  raw text;
  binding jsonb;
  expected_epoch uuid;
  expected_generation uuid;
  current_generation uuid;
  current_state text;
BEGIN
  raw=current_setting('cxthub.doc_catalog_collector',true);
  IF raw IS NULL OR octet_length(raw) NOT BETWEEN 1 AND 512 THEN
    RAISE EXCEPTION 'document catalog deletes require a bound collector' USING ERRCODE='23514';
  END IF;
  BEGIN
    binding=raw::jsonb;
    expected_epoch=(binding->>'epoch')::uuid;
    expected_generation=(binding->>'generation')::uuid;
  EXCEPTION WHEN data_exception THEN
    RAISE EXCEPTION 'document catalog collector binding is invalid' USING ERRCODE='23514';
  END;
  IF binding IS DISTINCT FROM jsonb_build_object('source',source,'epoch',expected_epoch,'generation',expected_generation)
    OR expected_epoch IS NULL OR expected_generation IS NULL THEN
    RAISE EXCEPTION 'document catalog collector binding is invalid' USING ERRCODE='23514';
  END IF;
  current_generation=cxthub_lock_doc_catalog_retirement(source,expected_epoch);
  SELECT state INTO current_state FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=source;
  IF current_state IS DISTINCT FROM 'retiring' OR current_generation IS DISTINCT FROM expected_generation THEN
    RAISE EXCEPTION 'document catalog collector binding is not live' USING ERRCODE='23514';
  END IF;
END;
$$;

CREATE OR REPLACE FUNCTION cxthub_guard_doc_catalog_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  generation uuid;
BEGIN
  IF TG_OP='DELETE' THEN
    IF EXISTS(SELECT 1 FROM doc_display_sources_v1 WHERE source_hash=OLD.source_hash) THEN
      RAISE EXCEPTION 'document catalog lifecycle is immutable' USING ERRCODE='0A000';
    END IF;
    RETURN OLD;
  ELSIF TG_OP='INSERT' THEN
    IF NEW.state<>'live' OR NEW.retire_generation IS NOT NULL OR NEW.retire_phase<>'' OR NEW.retire_scope<>'' THEN
      RAISE EXCEPTION 'document catalog lifecycle must start live' USING ERRCODE='0A000';
    END IF;
    RETURN NEW;
  ELSIF NEW IS NOT DISTINCT FROM OLD THEN
    RETURN NEW;
  END IF;
  IF NEW.source_hash IS DISTINCT FROM OLD.source_hash THEN
    RAISE EXCEPTION 'document catalog lifecycle is immutable' USING ERRCODE='0A000';
  END IF;
  IF OLD.state='live' AND NEW.state='retiring' AND NEW.catalog_epoch=OLD.catalog_epoch
    AND NEW.retire_phase='orders' AND NEW.retire_scope='' THEN
    generation=cxthub_lock_doc_catalog_retirement(OLD.source_hash,OLD.catalog_epoch);
    IF NEW.retire_generation=generation AND cxthub_doc_catalog_clear(OLD.source_hash,OLD.catalog_epoch,generation) THEN
      RETURN NEW;
    END IF;
  ELSIF OLD.state='retiring' AND NEW.catalog_epoch=OLD.catalog_epoch AND NEW.retire_generation=OLD.retire_generation THEN
    PERFORM cxthub_require_doc_catalog_collector(OLD.source_hash);
    IF NEW.state='retired' AND OLD.retire_phase='empty' AND NEW.retire_phase='empty' AND NEW.retire_scope=''
      AND cxthub_doc_catalog_empty(OLD.source_hash) THEN
      RETURN NEW;
    ELSIF NEW.state='retiring' THEN
      IF NEW.retire_phase=OLD.retire_phase THEN
        RETURN NEW;
      ELSIF OLD.retire_phase='orders' AND NEW.retire_phase='keys' AND NEW.retire_scope=''
        AND NOT EXISTS(SELECT 1 FROM doc_tool_order_sources_v1 WHERE source_hash=OLD.source_hash) THEN
        RETURN NEW;
      ELSIF OLD.retire_phase='keys' AND NEW.retire_phase='trees' AND NEW.retire_scope=''
        AND NOT EXISTS(SELECT 1 FROM doc_tool_key_sources_v1 keys JOIN doc_tool_tree_sources_v1 trees
          ON trees.scope=keys.tree_scope WHERE trees.source_hash=OLD.source_hash) THEN
        RETURN NEW;
      ELSIF OLD.retire_phase='trees' AND NEW.retire_phase='empty' AND NEW.retire_scope=''
        AND cxthub_doc_catalog_empty(OLD.source_hash) THEN
        RETURN NEW;
      END IF;
    END IF;
  ELSIF OLD.state='retired' AND NEW.state='live'
    AND NEW.retire_generation IS NULL AND NEW.retire_phase='' AND NEW.retire_scope=''
    AND coalesce(current_setting('cxthub.doc_catalog_restart',true),'')=OLD.source_hash||':'||OLD.catalog_epoch::text THEN
    PERFORM cxthub_require_doc_catalog_writer(OLD.source_hash,true);
    IF cxthub_doc_catalog_empty(OLD.source_hash) THEN
      LOOP
        NEW.catalog_epoch=gen_random_uuid();
        EXIT WHEN NEW.catalog_epoch<>OLD.catalog_epoch AND NEW.catalog_epoch<>'00000000-0000-0000-0000-000000000000'::uuid;
      END LOOP;
      RETURN NEW;
    END IF;
  END IF;
  RAISE EXCEPTION 'document catalog lifecycle is immutable' USING ERRCODE='0A000';
END;
$$;

CREATE FUNCTION cxthub_begin_doc_catalog_retirement(source text,expected_epoch uuid) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
  generation uuid;
  current_state text;
BEGIN
  generation=cxthub_lock_doc_catalog_retirement(source,expected_epoch);
  SELECT state INTO current_state FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=source;
  IF current_state<>'live' OR NOT cxthub_doc_catalog_clear(source,expected_epoch,generation) OR cxthub_doc_catalog_empty(source) THEN
    RETURN false;
  END IF;
  UPDATE doc_display_catalog_lifecycle_v1 SET state='retiring',retire_generation=generation,retire_phase='orders',retire_scope=''
    WHERE source_hash=source AND catalog_epoch=expected_epoch AND state='live';
  UPDATE doc_display_maintenance_v1 SET phase=CASE WHEN phase='idle' THEN 'index' ELSE phase END,due_at=clock_timestamp()
    WHERE source_hash=source;
  RETURN true;
END;
$$;

CREATE FUNCTION cxthub_finish_doc_catalog_retirement(source text,expected_epoch uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM cxthub_bind_doc_catalog_collector(source,expected_epoch);
  UPDATE doc_display_catalog_lifecycle_v1 SET state='retired',retire_scope=''
    WHERE source_hash=source AND catalog_epoch=expected_epoch AND state='retiring' AND retire_phase='empty';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog retirement is not complete' USING ERRCODE='23514';
  END IF;
END;
$$;

CREATE FUNCTION cxthub_restart_doc_catalog(source text,expected_epoch uuid,initial_hash text,initial_raw bytea) RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE
  binding jsonb;
  owner record;
  source_raw bytea;
  source_value jsonb;
  initial_value jsonb;
  fresh uuid;
BEGIN
  PERFORM cxthub_require_doc_catalog_writer(source,true);
  binding=current_setting('cxthub.doc_catalog_job',true)::jsonb;
  SELECT job.* INTO owner FROM doc_display_jobs_v1 job WHERE job.repo_id=binding->>'repo' AND job.doc_hash=binding->>'doc'
    AND job.event_index=(binding->>'event')::integer AND job.profile=binding->>'profile'
    AND job.version=(binding->>'version')::bigint AND job.state='running' AND job.lease_until>clock_timestamp() FOR UPDATE NOWAIT;
  IF NOT FOUND OR owner.profile<>'public-event-display-v1' OR initial_raw IS NULL OR octet_length(initial_raw) NOT BETWEEN 1 AND 2048
    OR initial_hash IS DISTINCT FROM 'sha256:'||encode(sha256(initial_raw),'hex') THEN
    RAISE EXCEPTION 'document catalog restart binding is invalid' USING ERRCODE='0A000';
  END IF;
  SELECT bytes INTO source_raw FROM doc_display_sources_v1 WHERE source_hash=source AND event_hash=owner.event_hash AND profile=owner.profile;
  BEGIN
    source_value=convert_from(source_raw,'UTF8')::jsonb;
    initial_value=convert_from(initial_raw,'UTF8')::jsonb;
  EXCEPTION WHEN data_exception THEN
    RAISE EXCEPTION 'document catalog restart binding is invalid' USING ERRCODE='0A000';
  END;
  IF source_raw IS NULL OR source IS DISTINCT FROM 'sha256:'||encode(sha256(source_raw),'hex')
    OR source_value->'version' IS DISTINCT FROM '1'::jsonb OR source_value->>'profile' IS DISTINCT FROM owner.profile
    OR source_value->>'event_hash' IS DISTINCT FROM owner.event_hash OR source_value->'event_bytes' IS DISTINCT FROM to_jsonb(owner.event_length)
    OR coalesce(source_value->>'kind','') NOT IN ('reasoning','message','tool_call','tool_result')
    OR initial_value IS DISTINCT FROM jsonb_build_object('version',1,'source',source_value,
      'phase',CASE WHEN source_value->>'kind' IN ('tool_call','tool_result') THEN 'tool_discovery' ELSE 'discovery' END)
    OR NOT cxthub_doc_catalog_empty(source) THEN
    RAISE EXCEPTION 'document catalog restart binding is invalid' USING ERRCODE='0A000';
  END IF;
  PERFORM set_config('cxthub.doc_catalog_restart',source||':'||expected_epoch::text,true);
  UPDATE doc_display_catalog_lifecycle_v1 SET state='live',retire_generation=NULL,retire_phase='',retire_scope=''
    WHERE source_hash=source AND catalog_epoch=expected_epoch AND state='retired' RETURNING catalog_epoch INTO fresh;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog epoch changed' USING ERRCODE='55000';
  END IF;
  UPDATE doc_display_jobs_v1 SET source_hash=source,preparation_hash=initial_hash,preparation=initial_raw,catalog_epoch=fresh
    WHERE repo_id=owner.repo_id AND doc_hash=owner.doc_hash AND event_index=owner.event_index AND profile=owner.profile
      AND state='running' AND version=owner.version AND lease_until>clock_timestamp();
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  PERFORM set_config('cxthub.doc_catalog_restart','',true);
  RETURN fresh;
END;
$$;

CREATE FUNCTION cxthub_verify_doc_catalog_restart() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  binding jsonb;
  preparation_raw bytea;
  preparation_hash text;
  preparation_value jsonb;
  source_raw bytea;
  source_value jsonb;
BEGIN
  IF OLD.state<>'retired' OR NEW.state<>'live' THEN RETURN NULL; END IF;
  PERFORM cxthub_require_doc_catalog_writer(NEW.source_hash,true);
  binding=current_setting('cxthub.doc_catalog_job',true)::jsonb;
  SELECT job.preparation,job.preparation_hash,owner.bytes INTO preparation_raw,preparation_hash,source_raw
    FROM doc_display_jobs_v1 job JOIN doc_display_sources_v1 owner ON owner.source_hash=job.source_hash
      AND owner.event_hash=job.event_hash AND owner.profile=job.profile
    WHERE job.repo_id=binding->>'repo' AND job.doc_hash=binding->>'doc' AND job.event_index=(binding->>'event')::integer
      AND job.profile=binding->>'profile' AND job.version=(binding->>'version')::bigint
      AND job.source_hash=NEW.source_hash AND job.catalog_epoch=NEW.catalog_epoch
      AND job.state='running' AND job.lease_until>clock_timestamp();
  IF preparation_raw IS NULL OR source_raw IS NULL OR octet_length(preparation_raw) NOT BETWEEN 1 AND 2048
    OR preparation_hash IS DISTINCT FROM 'sha256:'||encode(sha256(preparation_raw),'hex')
    OR NEW.source_hash IS DISTINCT FROM 'sha256:'||encode(sha256(source_raw),'hex') THEN
    RAISE EXCEPTION 'document catalog restart requires canonical initialization' USING ERRCODE='23514';
  END IF;
  BEGIN
    preparation_value=convert_from(preparation_raw,'UTF8')::jsonb;
    source_value=convert_from(source_raw,'UTF8')::jsonb;
  EXCEPTION WHEN data_exception THEN
    RAISE EXCEPTION 'document catalog restart requires canonical initialization' USING ERRCODE='23514';
  END;
  IF preparation_value IS DISTINCT FROM jsonb_build_object('version',1,'source',source_value,
    'phase',CASE WHEN source_value->>'kind' IN ('tool_call','tool_result') THEN 'tool_discovery' ELSE 'discovery' END) THEN
    RAISE EXCEPTION 'document catalog restart requires canonical initialization' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER doc_display_catalog_lifecycle_v1_restart_complete
  AFTER UPDATE ON doc_display_catalog_lifecycle_v1 DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW WHEN (OLD.state='retired' AND NEW.state='live') EXECUTE FUNCTION cxthub_verify_doc_catalog_restart();

CREATE FUNCTION cxthub_guard_doc_tool_catalog_delete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  owner text;
  incoming boolean;
BEGIN
  CASE TG_TABLE_NAME
  WHEN 'doc_tool_tree_sources_v1','doc_tool_order_sources_v1' THEN owner=OLD.source_hash;
  WHEN 'doc_tool_tree_nodes_v1' THEN SELECT source_hash INTO owner FROM doc_tool_tree_sources_v1 WHERE scope=OLD.scope;
  WHEN 'doc_tool_key_sources_v1' THEN SELECT source_hash INTO owner FROM doc_tool_tree_sources_v1 WHERE scope=OLD.tree_scope;
  WHEN 'doc_tool_key_observations_v1','doc_tool_key_members_v1' THEN
    SELECT tree.source_hash INTO owner FROM doc_tool_key_sources_v1 keys JOIN doc_tool_tree_sources_v1 tree ON tree.scope=keys.tree_scope WHERE keys.scope=OLD.scope;
  WHEN 'doc_tool_order_plans_v1' THEN SELECT source_hash INTO owner FROM doc_tool_order_sources_v1 WHERE scope=OLD.scope;
  END CASE;
  IF owner IS NULL OR NOT EXISTS(SELECT 1 FROM doc_display_sources_v1 WHERE source_hash=owner) THEN
    RETURN OLD;
  END IF;
  PERFORM cxthub_require_doc_catalog_collector(owner);
  CASE TG_TABLE_NAME
  WHEN 'doc_tool_tree_sources_v1' THEN
    incoming=EXISTS(SELECT 1 FROM doc_tool_tree_nodes_v1 WHERE scope=OLD.scope)
      OR EXISTS(SELECT 1 FROM doc_tool_key_sources_v1 WHERE tree_scope=OLD.scope)
      OR EXISTS(SELECT 1 FROM doc_tool_order_sources_v1 WHERE tree_scope=OLD.scope);
  WHEN 'doc_tool_tree_nodes_v1' THEN
    incoming=EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE tree_scope=OLD.scope AND node_from=OLD.node_from)
      OR EXISTS(SELECT 1 FROM doc_tool_order_plans_v1 WHERE tree_scope=OLD.scope AND parent_from=OLD.node_from);
  WHEN 'doc_tool_key_sources_v1' THEN
    incoming=EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=OLD.scope)
      OR EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=OLD.scope)
      OR EXISTS(SELECT 1 FROM doc_tool_order_sources_v1 WHERE keys_scope=OLD.scope);
  WHEN 'doc_tool_key_members_v1' THEN
    incoming=EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=OLD.scope AND parent_from=OLD.parent_from
      AND digest=OLD.digest AND node_from<>OLD.first_from AND node_from<>OLD.last_from);
  WHEN 'doc_tool_key_observations_v1' THEN
    incoming=EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=OLD.scope AND first_from=OLD.node_from)
      OR EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=OLD.scope AND last_from=OLD.node_from);
  WHEN 'doc_tool_order_sources_v1' THEN incoming=EXISTS(SELECT 1 FROM doc_tool_order_plans_v1 WHERE scope=OLD.scope);
  ELSE incoming=false;
  END CASE;
  IF incoming THEN
    RAISE EXCEPTION 'document catalog retirement cannot cascade private children' USING ERRCODE='23514';
  END IF;
  RETURN OLD;
END;
$$;

CREATE TRIGGER doc_tool_tree_sources_v1_a_catalog_delete BEFORE DELETE ON doc_tool_tree_sources_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_tree_nodes_v1_a_catalog_delete BEFORE DELETE ON doc_tool_tree_nodes_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_key_sources_v1_a_catalog_delete BEFORE DELETE ON doc_tool_key_sources_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_key_observations_v1_a_catalog_delete BEFORE DELETE ON doc_tool_key_observations_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_key_members_v1_a_catalog_delete BEFORE DELETE ON doc_tool_key_members_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_order_sources_v1_a_catalog_delete BEFORE DELETE ON doc_tool_order_sources_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();
CREATE TRIGGER doc_tool_order_plans_v1_a_catalog_delete BEFORE DELETE ON doc_tool_order_plans_v1 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_delete();

CREATE OR REPLACE FUNCTION cxthub_guard_doc_tool_key_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_scope text;
  next_scope text;
  source_state text;
  owner text;
BEGIN
  IF TG_OP='DELETE' THEN
    SELECT tree.source_hash INTO owner FROM doc_tool_key_sources_v1 keys JOIN doc_tool_tree_sources_v1 tree ON tree.scope=keys.tree_scope WHERE keys.scope=OLD.scope;
    IF owner IS NOT NULL AND EXISTS(SELECT 1 FROM doc_display_sources_v1 WHERE source_hash=owner) THEN
      PERFORM cxthub_require_doc_catalog_collector(owner);
    END IF;
    RETURN NULL;
  END IF;
  IF TG_OP<>'INSERT' THEN previous_scope=OLD.scope; END IF;
  next_scope=NEW.scope;
  FOR source_state IN SELECT source.state FROM doc_tool_key_sources_v1 source JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope
    WHERE source.scope=previous_scope OR source.scope=next_scope ORDER BY source.scope FOR UPDATE OF source
  LOOP
    IF source_state='complete' THEN
      IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN CONTINUE; END IF;
      RAISE EXCEPTION 'completed tool key rows are immutable' USING ERRCODE='23514';
    END IF;
  END LOOP;
  RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION cxthub_guard_doc_tool_order_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  previous_scope text;
  next_scope text;
  source_state text;
  owner text;
BEGIN
  IF TG_OP='DELETE' THEN
    SELECT source_hash INTO owner FROM doc_tool_order_sources_v1 WHERE scope=OLD.scope;
    IF owner IS NOT NULL AND EXISTS(SELECT 1 FROM doc_display_sources_v1 WHERE source_hash=owner) THEN
      PERFORM cxthub_require_doc_catalog_collector(owner);
    END IF;
    RETURN NULL;
  END IF;
  IF TG_OP<>'INSERT' THEN previous_scope=OLD.scope; END IF;
  next_scope=NEW.scope;
  FOR source_state IN SELECT source.state FROM doc_tool_order_sources_v1 source JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope
    WHERE source.scope=previous_scope OR source.scope=next_scope ORDER BY source.scope FOR UPDATE OF source
  LOOP
    IF TG_OP='UPDATE' THEN
      IF NEW IS NOT DISTINCT FROM OLD THEN CONTINUE; END IF;
      RAISE EXCEPTION 'ready tool order plans are immutable' USING ERRCODE='23514';
    END IF;
    IF source_state='complete' THEN RAISE EXCEPTION 'completed tool order plans are immutable' USING ERRCODE='23514'; END IF;
  END LOOP;
  RETURN NULL;
END;
$$;
