SET LOCAL lock_timeout='250ms';

LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1,
  doc_tool_tree_sources_v1,doc_tool_tree_nodes_v1,doc_tool_key_sources_v1,
  doc_tool_key_observations_v1,doc_tool_key_members_v1,
  doc_tool_order_sources_v1,doc_tool_order_plans_v1
  IN SHARE ROW EXCLUSIVE MODE NOWAIT;

UPDATE doc_display_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL,
  reason='',next_attempt=LEAST(next_attempt,clock_timestamp()) WHERE state='running';

CREATE FUNCTION cxthub_guard_doc_catalog_claim() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ((NEW.profile='public-event-display-v1' AND NEW.worker_protocol=5)
    OR (NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2)) IS NOT TRUE THEN
    RETURN NEW;
  END IF;
  IF NEW.state<>'running' THEN
    RETURN NEW;
  END IF;
  IF TG_OP='UPDATE' THEN
    IF OLD.state='running' AND OLD.version=NEW.version AND OLD.profile=NEW.profile AND OLD.worker_protocol=NEW.worker_protocol THEN
      IF TG_NARGS>0 THEN
        PERFORM cxthub_require_doc_catalog_writer(OLD.source_hash,true,jsonb_build_object(
          'repo',OLD.repo_id,'doc',OLD.doc_hash,'event',OLD.event_index,'profile',OLD.profile,'version',OLD.version));
      END IF;
      RETURN NEW;
    END IF;
  END IF;
  IF coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')<>'1' THEN
    RAISE EXCEPTION 'document catalog claims require writer protocol 1' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_display_z_catalog_claim
  BEFORE INSERT OR UPDATE OF state,version,profile,worker_protocol ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_claim();

CREATE TRIGGER guard_doc_display_z_catalog_renew
  BEFORE UPDATE OF lease_until ON doc_display_jobs_v1
  FOR EACH ROW WHEN (NEW.state='running') EXECUTE FUNCTION cxthub_guard_doc_catalog_claim('renew');

CREATE FUNCTION cxthub_bind_doc_catalog_writer(repository text,document text,event_index integer,display_profile text,claim_version bigint)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  event text;
  previous_source text;
  locked_source text;
BEGIN
  SELECT job.event_hash,job.source_hash INTO event,previous_source FROM doc_display_jobs_v1 job
  WHERE job.repo_id=repository AND job.doc_hash=document AND job.event_index=cxthub_bind_doc_catalog_writer.event_index
    AND job.profile=display_profile AND job.version=claim_version AND job.state='running'
    AND job.lease_until>clock_timestamp()
    AND ((job.profile='public-event-display-v1' AND job.worker_protocol=5)
      OR (job.profile='tool-result-string-v1' AND job.worker_protocol=2))
  FOR UPDATE NOWAIT;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  PERFORM * FROM cxthub_lock_doc_catalog_affinities(ARRAY[event],ARRAY[display_profile],ARRAY[previous_source]);
  FOR locked_source IN
    SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=event AND profile=display_profile
    UNION SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=previous_source
    ORDER BY source_hash
  LOOP
    PERFORM source_hash FROM doc_display_sources_v1 WHERE source_hash=locked_source FOR NO KEY UPDATE;
  END LOOP;
  PERFORM set_config('cxthub.doc_catalog_protocol','1',true);
  PERFORM set_config('cxthub.doc_catalog_job',jsonb_build_object(
    'repo',repository,'doc',document,'event',event_index,'profile',display_profile,'version',claim_version)::text,true);
END;
$$;

CREATE FUNCTION cxthub_require_doc_catalog_writer(source text,allow_unbound boolean DEFAULT false,expected_job jsonb DEFAULT NULL)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  raw text;
  binding jsonb;
  binding_event integer;
  binding_version bigint;
  live boolean;
  claim_event text;
  claim_profile text;
  claim_lease timestamptz;
  current_owner text;
BEGIN
  raw=current_setting('cxthub.doc_catalog_job',true);
  IF coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')<>'1' OR raw IS NULL OR octet_length(raw)>1024 THEN
    RAISE EXCEPTION 'document catalog writes require a bound writer' USING ERRCODE='0A000';
  END IF;
  BEGIN
    binding=raw::jsonb;
    binding_event=(binding->>'event')::integer;
    binding_version=(binding->>'version')::bigint;
  EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
    RAISE EXCEPTION 'document catalog writer binding is invalid' USING ERRCODE='0A000';
  END;
  IF expected_job IS NOT NULL AND binding IS DISTINCT FROM expected_job THEN
    RAISE EXCEPTION 'document catalog writer binding is invalid' USING ERRCODE='0A000';
  END IF;
    SELECT job.event_hash,job.profile,job.lease_until INTO claim_event,claim_profile,claim_lease FROM doc_display_jobs_v1 job
      WHERE job.repo_id=binding->>'repo' AND job.doc_hash=binding->>'doc'
        AND job.event_index=binding_event AND job.profile=binding->>'profile'
        AND job.version=binding_version AND job.state='running' AND job.lease_until>clock_timestamp()
        AND ((source IS NULL AND allow_unbound AND job.source_hash IS NULL)
          OR ((job.source_hash=source OR (allow_unbound AND job.source_hash IS NULL))
            AND EXISTS(SELECT 1 FROM doc_display_sources_v1 owner WHERE owner.event_hash=job.event_hash
              AND owner.profile=job.profile AND owner.source_hash=source)))
        AND ((job.profile='public-event-display-v1' AND job.worker_protocol=5)
          OR (job.profile='tool-result-string-v1' AND job.worker_protocol=2)) FOR UPDATE NOWAIT;
    live=FOUND;
  IF live IS NOT TRUE THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  PERFORM * FROM cxthub_lock_doc_catalog_affinities(ARRAY[claim_event],ARRAY[claim_profile],ARRAY[source]);
  SELECT source_hash INTO current_owner FROM doc_display_sources_v1 WHERE event_hash=claim_event AND profile=claim_profile FOR NO KEY UPDATE NOWAIT;
  IF source IS NOT NULL AND current_owner IS DISTINCT FROM source THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  IF claim_lease<=clock_timestamp() THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
END;
$$;

CREATE FUNCTION cxthub_guard_doc_catalog_job_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.state<>'running' THEN
    RETURN NEW;
  END IF;
  IF NEW.state='running' AND NEW.version=OLD.version+1 AND NEW.attempts=OLD.attempts+1
    AND OLD.lease_until<=clock_timestamp() AND NEW.lease_until>clock_timestamp()
    AND coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')='1'
    AND (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.event_offset,NEW.event_length,NEW.event_seq,NEW.event_role,
      NEW.source_id,NEW.source_version,NEW.range_generation,NEW.profile,NEW.worker_protocol,NEW.source_hash,NEW.checkpoint_hash,
      NEW.checkpoint,NEW.preparation_hash,NEW.preparation,NEW.discovery_hash,NEW.discovery,NEW.created_at,NEW.next_attempt)
      IS NOT DISTINCT FROM
      (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.event_offset,OLD.event_length,OLD.event_seq,OLD.event_role,
      OLD.source_id,OLD.source_version,OLD.range_generation,OLD.profile,OLD.worker_protocol,OLD.source_hash,OLD.checkpoint_hash,
      OLD.checkpoint,OLD.preparation_hash,OLD.preparation,OLD.discovery_hash,OLD.discovery,OLD.created_at,OLD.next_attempt) THEN
    RETURN NEW;
  END IF;
  IF NEW.state IN ('waiting','retrying','rejected') AND NEW.source_hash IS NOT DISTINCT FROM OLD.source_hash
    AND NEW.checkpoint_hash IS NOT DISTINCT FROM OLD.checkpoint_hash AND NEW.checkpoint IS NOT DISTINCT FROM OLD.checkpoint
    AND NEW.preparation_hash IS NOT DISTINCT FROM OLD.preparation_hash AND NEW.preparation IS NOT DISTINCT FROM OLD.preparation
    AND NEW.discovery_hash IS NOT DISTINCT FROM OLD.discovery_hash AND NEW.discovery IS NOT DISTINCT FROM OLD.discovery THEN
    RETURN NEW;
  END IF;
  IF NEW.state='waiting' AND NEW.version>OLD.version AND NEW.source_hash IS NULL AND NEW.checkpoint_hash IS NULL
    AND NEW.checkpoint IS NULL AND NEW.preparation_hash IS NULL AND NEW.preparation IS NULL
    AND NEW.discovery_hash IS NULL AND NEW.discovery IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM cxthub_require_doc_catalog_writer(NEW.source_hash,true,jsonb_build_object(
    'repo',OLD.repo_id,'doc',OLD.doc_hash,'event',OLD.event_index,'profile',OLD.profile,'version',OLD.version));
  RETURN NEW;
END;
$$;

CREATE TRIGGER guard_doc_display_zz_catalog_write
  BEFORE UPDATE OF source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,discovery_hash,discovery,state ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_job_write();

CREATE FUNCTION cxthub_guard_doc_tool_catalog_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  owner text;
BEGIN
  IF TG_OP='UPDATE' AND NEW.scope IS DISTINCT FROM OLD.scope THEN
    RAISE EXCEPTION 'document catalog scope is immutable' USING ERRCODE='23514';
  END IF;
  CASE TG_TABLE_NAME
  WHEN 'doc_tool_tree_sources_v1' THEN
    IF TG_OP='UPDATE' AND NEW.source_hash IS DISTINCT FROM OLD.source_hash THEN
      RAISE EXCEPTION 'document catalog owner is immutable' USING ERRCODE='23514';
    END IF;
    owner=NEW.source_hash;
  WHEN 'doc_tool_tree_nodes_v1' THEN
    SELECT source_hash INTO owner FROM doc_tool_tree_sources_v1 WHERE scope=NEW.scope;
  WHEN 'doc_tool_key_sources_v1' THEN
    IF TG_OP='UPDATE' AND NEW.tree_scope IS DISTINCT FROM OLD.tree_scope THEN
      RAISE EXCEPTION 'document catalog owner is immutable' USING ERRCODE='23514';
    END IF;
    SELECT source_hash INTO owner FROM doc_tool_tree_sources_v1 WHERE scope=NEW.tree_scope;
  WHEN 'doc_tool_key_observations_v1','doc_tool_key_members_v1' THEN
    SELECT tree.source_hash INTO owner FROM doc_tool_key_sources_v1 keys
      JOIN doc_tool_tree_sources_v1 tree ON tree.scope=keys.tree_scope WHERE keys.scope=NEW.scope;
  WHEN 'doc_tool_order_sources_v1' THEN
    IF TG_OP='UPDATE' AND (NEW.source_hash,NEW.tree_scope,NEW.keys_scope) IS DISTINCT FROM (OLD.source_hash,OLD.tree_scope,OLD.keys_scope) THEN
      RAISE EXCEPTION 'document catalog owner is immutable' USING ERRCODE='23514';
    END IF;
    SELECT tree.source_hash INTO owner FROM doc_tool_tree_sources_v1 tree
      JOIN doc_tool_key_sources_v1 keys ON keys.tree_scope=tree.scope
      WHERE tree.scope=NEW.tree_scope AND keys.scope=NEW.keys_scope AND tree.source_hash=NEW.source_hash;
  WHEN 'doc_tool_order_plans_v1' THEN
    SELECT source_hash INTO owner FROM doc_tool_order_sources_v1 WHERE scope=NEW.scope AND tree_scope=NEW.tree_scope;
  ELSE
    RAISE EXCEPTION 'unknown document catalog' USING ERRCODE='0A000';
  END CASE;
  PERFORM cxthub_require_doc_catalog_writer(owner);
  RETURN NEW;
END;
$$;

CREATE TRIGGER doc_tool_tree_sources_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_tree_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_tree_nodes_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_tree_nodes_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_key_sources_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_key_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_key_observations_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_key_observations_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_key_members_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_key_members_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_order_sources_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_order_sources_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
CREATE TRIGGER doc_tool_order_plans_v1_a_catalog_writer BEFORE INSERT OR UPDATE ON doc_tool_order_plans_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_tool_catalog_write();
