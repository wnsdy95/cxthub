SET LOCAL lock_timeout='250ms';

LOCK TABLE doc_display_jobs_v1,doc_display_sources_v1,doc_display_catalog_admissions_v1,
  doc_tool_tree_sources_v1,doc_tool_tree_nodes_v1,doc_tool_key_sources_v1,
  doc_tool_key_observations_v1,doc_tool_key_members_v1,
  doc_tool_order_sources_v1,doc_tool_order_plans_v1
  IN EXCLUSIVE MODE NOWAIT;

UPDATE doc_display_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL,
  reason='',next_attempt=LEAST(next_attempt,clock_timestamp()) WHERE state='running';

CREATE OR REPLACE FUNCTION cxthub_bind_doc_catalog_writer(repository text,document text,event_index integer,display_profile text,claim_version bigint)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'document catalog writers require calculation protocol 2' USING ERRCODE='0A000';
END;
$$;

CREATE FUNCTION cxthub_bind_doc_catalog_control(repository text,document text,event_index integer,display_profile text,claim_version bigint)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  event text;
  previous_source text;
  locked_source text;
  claim_lease timestamptz;
BEGIN
  SELECT job.event_hash,job.source_hash,job.lease_until INTO event,previous_source,claim_lease FROM doc_display_jobs_v1 job
  WHERE job.repo_id=repository AND job.doc_hash=document AND job.event_index=cxthub_bind_doc_catalog_control.event_index
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
    PERFORM source_hash FROM doc_display_sources_v1 WHERE source_hash=locked_source FOR NO KEY UPDATE NOWAIT;
  END LOOP;
  IF claim_lease<=clock_timestamp() THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  PERFORM set_config('cxthub.doc_catalog_protocol','2',true);
  PERFORM set_config('cxthub.doc_catalog_job',jsonb_build_object(
    'repo',repository,'doc',document,'event',event_index,'profile',display_profile,'version',claim_version)::text,true);
  PERFORM set_config('cxthub.doc_catalog_use','',true);
END;
$$;

CREATE FUNCTION cxthub_bind_doc_catalog_calculation(repository text,document text,event_index integer,display_profile text,claim_version bigint,
  expected_source text,expected_generation uuid)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  current_owner text;
  current_generation uuid;
BEGIN
  PERFORM cxthub_bind_doc_catalog_control(repository,document,event_index,display_profile,claim_version);
  IF expected_source IS NULL OR expected_source !~ '^sha256:[0-9a-f]{64}$'
    OR expected_generation IS NULL OR expected_generation='00000000-0000-0000-0000-000000000000'::uuid THEN
    RAISE EXCEPTION 'document catalog calculation binding is invalid' USING ERRCODE='0A000';
  END IF;
  SELECT owner.source_hash,admission.generation INTO current_owner,current_generation FROM doc_display_jobs_v1 job
    JOIN doc_display_sources_v1 owner ON owner.event_hash=job.event_hash AND owner.profile=job.profile AND owner.source_hash=job.source_hash
    JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=owner.event_hash
      AND admission.profile=owner.profile AND admission.source_hash=owner.source_hash
  WHERE job.repo_id=repository AND job.doc_hash=document AND job.event_index=cxthub_bind_doc_catalog_calculation.event_index
    AND job.profile=display_profile AND job.version=claim_version AND job.state='running' AND job.lease_until>clock_timestamp();
  IF current_owner IS DISTINCT FROM expected_source OR current_generation IS NULL
    OR current_generation='00000000-0000-0000-0000-000000000000'::uuid THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  IF current_generation<>expected_generation THEN
    RAISE EXCEPTION 'document catalog source incarnation changed' USING ERRCODE='55000';
  END IF;
  PERFORM set_config('cxthub.doc_catalog_use',jsonb_build_object('source',expected_source,'generation',expected_generation)::text,true);
END;
$$;

CREATE OR REPLACE FUNCTION cxthub_require_doc_catalog_writer(source text,allow_unbound boolean DEFAULT false,expected_job jsonb DEFAULT NULL)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  raw text;
  binding jsonb;
  binding_event integer;
  binding_version bigint;
  claim_event text;
  claim_profile text;
  claim_lease timestamptz;
  current_owner text;
  current_generation uuid;
  calculation jsonb;
  calculation_generation uuid;
BEGIN
  raw=current_setting('cxthub.doc_catalog_job',true);
  IF coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')<>'2'
    OR raw IS NULL OR octet_length(raw) NOT BETWEEN 1 AND 1024 THEN
    RAISE EXCEPTION 'document catalog writes require a bound writer' USING ERRCODE='0A000';
  END IF;
  BEGIN
    binding=raw::jsonb;
    binding_event=(binding->>'event')::integer;
    binding_version=(binding->>'version')::bigint;
  EXCEPTION WHEN data_exception THEN
    RAISE EXCEPTION 'document catalog writer binding is invalid' USING ERRCODE='0A000';
  END;
  IF binding_event IS NULL OR binding_event<0 OR binding_version IS NULL OR binding_version<0
    OR coalesce(binding->>'repo','')='' OR coalesce(binding->>'doc','')='' OR coalesce(binding->>'profile','')=''
    OR binding IS DISTINCT FROM jsonb_build_object('repo',binding->>'repo','doc',binding->>'doc',
      'event',binding_event,'profile',binding->>'profile','version',binding_version)
    OR (expected_job IS NOT NULL AND binding IS DISTINCT FROM expected_job) THEN
    RAISE EXCEPTION 'document catalog writer binding is invalid' USING ERRCODE='0A000';
  END IF;
  SELECT job.event_hash,job.profile,job.lease_until INTO claim_event,claim_profile,claim_lease FROM doc_display_jobs_v1 job
  WHERE job.repo_id=binding->>'repo' AND job.doc_hash=binding->>'doc' AND job.event_index=binding_event
    AND job.profile=binding->>'profile' AND job.version=binding_version AND job.state='running' AND job.lease_until>clock_timestamp()
    AND ((source IS NULL AND allow_unbound AND job.source_hash IS NULL)
      OR ((job.source_hash=source OR (allow_unbound AND job.source_hash IS NULL))
        AND EXISTS(SELECT 1 FROM doc_display_sources_v1 owner WHERE owner.event_hash=job.event_hash
          AND owner.profile=job.profile AND owner.source_hash=source)))
    AND ((job.profile='public-event-display-v1' AND job.worker_protocol=5)
      OR (job.profile='tool-result-string-v1' AND job.worker_protocol=2)) FOR UPDATE NOWAIT;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  PERFORM * FROM cxthub_lock_doc_catalog_affinities(ARRAY[claim_event],ARRAY[claim_profile],ARRAY[source]);
  SELECT source_hash INTO current_owner FROM doc_display_sources_v1
    WHERE event_hash=claim_event AND profile=claim_profile FOR NO KEY UPDATE NOWAIT;
  IF (source IS NOT NULL AND current_owner IS DISTINCT FROM source) OR claim_lease<=clock_timestamp() THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  IF allow_unbound IS TRUE THEN
    RETURN;
  END IF;
  raw=current_setting('cxthub.doc_catalog_use',true);
  IF raw IS NULL OR octet_length(raw) NOT BETWEEN 1 AND 1024 THEN
    RAISE EXCEPTION 'document catalog writes require a bound calculation' USING ERRCODE='0A000';
  END IF;
  BEGIN
    calculation=raw::jsonb;
    calculation_generation=(calculation->>'generation')::uuid;
  EXCEPTION WHEN data_exception THEN
    RAISE EXCEPTION 'document catalog calculation binding is invalid' USING ERRCODE='0A000';
  END;
  IF calculation_generation IS NULL OR calculation_generation='00000000-0000-0000-0000-000000000000'::uuid
    OR coalesce(calculation->>'source','') !~ '^sha256:[0-9a-f]{64}$'
    OR calculation IS DISTINCT FROM jsonb_build_object('source',calculation->>'source','generation',calculation_generation) THEN
    RAISE EXCEPTION 'document catalog calculation binding is invalid' USING ERRCODE='0A000';
  END IF;
  IF source IS NULL OR current_owner IS DISTINCT FROM calculation->>'source' THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  SELECT generation INTO current_generation FROM doc_display_catalog_admissions_v1
    WHERE event_hash=claim_event AND profile=claim_profile AND source_hash=current_owner;
  IF current_generation IS NULL OR current_generation='00000000-0000-0000-0000-000000000000'::uuid THEN
    RAISE EXCEPTION 'document catalog writer claim is not live' USING ERRCODE='55000';
  END IF;
  IF current_generation<>calculation_generation THEN
    RAISE EXCEPTION 'document catalog source incarnation changed' USING ERRCODE='55000';
  END IF;
END;
$$;

CREATE OR REPLACE FUNCTION cxthub_guard_doc_catalog_claim() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ((NEW.profile='public-event-display-v1' AND NEW.worker_protocol=5)
    OR (NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2)) IS NOT TRUE OR NEW.state<>'running' THEN
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
  IF coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')<>'2' THEN
    RAISE EXCEPTION 'document catalog claims require writer protocol 2' USING ERRCODE='0A000';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION cxthub_guard_doc_catalog_job_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  progress_unchanged boolean;
  source_value jsonb;
  preparation_value jsonb;
  source_raw bytea;
  peer_hash text;
  peer_raw bytea;
  published_hash text;
  published_raw bytea;
  published_value jsonb;
BEGIN
  IF TG_OP='INSERT' THEN
    IF NEW.source_hash IS NOT NULL OR NEW.checkpoint_hash IS NOT NULL OR NEW.checkpoint IS NOT NULL
      OR NEW.preparation_hash IS NOT NULL OR NEW.preparation IS NOT NULL OR NEW.discovery_hash IS NOT NULL OR NEW.discovery IS NOT NULL THEN
      RAISE EXCEPTION 'document display jobs must be inserted without progress' USING ERRCODE='0A000';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.state='waiting' AND NEW.version>OLD.version AND NEW.source_hash IS NULL AND NEW.checkpoint_hash IS NULL
    AND NEW.checkpoint IS NULL AND NEW.preparation_hash IS NULL AND NEW.preparation IS NULL
    AND NEW.discovery_hash IS NULL AND NEW.discovery IS NULL THEN
    RETURN NEW;
  END IF;
  progress_unchanged=(NEW.checkpoint_hash,NEW.checkpoint,NEW.preparation_hash,NEW.preparation,NEW.discovery_hash,NEW.discovery)
    IS NOT DISTINCT FROM (OLD.checkpoint_hash,OLD.checkpoint,OLD.preparation_hash,OLD.preparation,OLD.discovery_hash,OLD.discovery);
  IF NEW.state IN ('waiting','retrying','rejected') AND NEW.source_hash IS NOT DISTINCT FROM OLD.source_hash AND progress_unchanged THEN
    RETURN NEW;
  END IF;
  IF NEW.state='running' AND NEW.version=OLD.version+1 AND NEW.attempts=OLD.attempts+1 AND NEW.reason=''
    AND (OLD.state IN ('waiting','retrying') OR (OLD.state='running' AND OLD.lease_until<=clock_timestamp()))
    AND NEW.lease_until>clock_timestamp() AND coalesce(current_setting('cxthub.doc_catalog_protocol',true),'')='2'
    AND (NEW.repo_id,NEW.doc_hash,NEW.event_index,NEW.event_hash,NEW.event_offset,NEW.event_length,NEW.event_seq,NEW.event_role,
      NEW.source_id,NEW.source_version,NEW.range_generation,NEW.profile,NEW.worker_protocol,NEW.source_hash,NEW.checkpoint_hash,
      NEW.checkpoint,NEW.preparation_hash,NEW.preparation,NEW.discovery_hash,NEW.discovery,NEW.created_at,NEW.next_attempt)
      IS NOT DISTINCT FROM
      (OLD.repo_id,OLD.doc_hash,OLD.event_index,OLD.event_hash,OLD.event_offset,OLD.event_length,OLD.event_seq,OLD.event_role,
      OLD.source_id,OLD.source_version,OLD.range_generation,OLD.profile,OLD.worker_protocol,OLD.source_hash,OLD.checkpoint_hash,
      OLD.checkpoint,OLD.preparation_hash,OLD.preparation,OLD.discovery_hash,OLD.discovery,OLD.created_at,OLD.next_attempt) THEN
    RETURN NEW;
  END IF;
  IF OLD.state<>'running' THEN
    IF NEW.state<>'running' AND (NEW.state<>'completed' OR OLD.state='completed') AND progress_unchanged THEN
      RETURN NEW;
    END IF;
    RAISE EXCEPTION 'inactive document display jobs cannot change progress' USING ERRCODE='0A000';
  END IF;
  PERFORM cxthub_require_doc_catalog_writer(NEW.source_hash,true,jsonb_build_object(
    'repo',OLD.repo_id,'doc',OLD.doc_hash,'event',OLD.event_index,'profile',OLD.profile,'version',OLD.version));
  IF NEW.state NOT IN ('running','completed') OR NEW.version<>OLD.version
    OR (NEW.event_hash,NEW.event_offset,NEW.event_length,NEW.event_seq,NEW.event_role,NEW.source_id,NEW.source_version,
      NEW.range_generation,NEW.profile,NEW.worker_protocol) IS DISTINCT FROM
      (OLD.event_hash,OLD.event_offset,OLD.event_length,OLD.event_seq,OLD.event_role,OLD.source_id,OLD.source_version,
      OLD.range_generation,OLD.profile,OLD.worker_protocol) THEN
    RAISE EXCEPTION 'document catalog writer binding is invalid' USING ERRCODE='0A000';
  END IF;
  IF NEW.source_hash IS NOT DISTINCT FROM OLD.source_hash AND progress_unchanged THEN
    RETURN NEW;
  END IF;
  IF NEW.state='completed' THEN
    RAISE EXCEPTION 'document display completion cannot change progress' USING ERRCODE='0A000';
  END IF;
  IF NEW.profile='tool-result-string-v1' AND NEW.worker_protocol=2 THEN
    RETURN NEW;
  END IF;
  IF coalesce(current_setting('cxthub.doc_catalog_use',true),'')<>'' THEN
    PERFORM cxthub_require_doc_catalog_writer(NEW.source_hash);
    RETURN NEW;
  END IF;
  IF NEW.preparation IS NOT NULL AND octet_length(NEW.preparation) BETWEEN 1 AND 2048
    AND NEW.preparation_hash='sha256:'||encode(sha256(NEW.preparation),'hex') THEN
    SELECT bytes INTO source_raw FROM doc_display_sources_v1
      WHERE event_hash=OLD.event_hash AND profile=OLD.profile AND source_hash=NEW.source_hash;
    BEGIN
      source_value=convert_from(source_raw,'UTF8')::jsonb;
      preparation_value=convert_from(NEW.preparation,'UTF8')::jsonb;
    EXCEPTION WHEN data_exception THEN
      RAISE EXCEPTION 'document display preparation binding is invalid' USING ERRCODE='0A000';
    END;
    IF NEW.source_hash='sha256:'||encode(sha256(source_raw),'hex')
      AND source_value->'version'='1'::jsonb AND source_value->>'profile'=OLD.profile
      AND source_value->>'event_hash'=OLD.event_hash AND source_value->'event_bytes'=to_jsonb(OLD.event_length)
      AND source_value->>'kind' IN ('reasoning','message','tool_call','tool_result') THEN
      IF OLD.preparation IS NULL AND preparation_value=jsonb_build_object('version',1,'source',source_value,
        'phase',CASE WHEN source_value->>'kind' IN ('tool_call','tool_result') THEN 'tool_discovery' ELSE 'discovery' END) THEN
        RETURN NEW;
      END IF;
      SELECT preparation_hash,preparation INTO peer_hash,peer_raw FROM doc_display_jobs_v1
        WHERE source_hash=NEW.source_hash AND profile='public-event-display-v1' AND worker_protocol=5 AND state='completed'
          AND preparation_hash IS NOT NULL AND preparation IS NOT NULL
        ORDER BY repo_id,doc_hash,event_index LIMIT 1;
      IF peer_hash=NEW.preparation_hash AND peer_raw=NEW.preparation THEN
        SELECT generation_hash,bytes INTO published_hash,published_raw FROM doc_display_generations_v1
          WHERE event_hash=OLD.event_hash AND profile=OLD.profile AND source_hash=NEW.source_hash;
        BEGIN
          published_value=convert_from(published_raw,'UTF8')::jsonb;
        EXCEPTION WHEN data_exception THEN
          RAISE EXCEPTION 'document display publication binding is invalid' USING ERRCODE='0A000';
        END;
        IF preparation_value->'version'='1'::jsonb AND preparation_value->>'phase'='completed'
          AND preparation_value->'source'=source_value AND preparation_value->>'generation'=published_hash
          AND published_hash='sha256:'||encode(sha256(published_raw),'hex')
          AND published_value->'version'='1'::jsonb AND published_value->'source'->'version'='1'::jsonb
          AND published_value->'source'->>'profile'=OLD.profile AND published_value->'source'->>'event_hash'=OLD.event_hash
          AND published_value->'source'->'event_bytes'=source_value->'event_bytes'
          AND preparation_value->'plan'=published_value->'source'->'plan'
          AND (source_value->'empty_reasoning'='false'::jsonb OR published_value->'source'->'text_bytes'='0'::jsonb) THEN
          RETURN NEW;
        END IF;
      END IF;
    END IF;
  END IF;
  PERFORM cxthub_require_doc_catalog_writer(NEW.source_hash);
  RETURN NEW;
END;
$$;

DROP TRIGGER guard_doc_display_zz_catalog_write ON doc_display_jobs_v1;
CREATE TRIGGER guard_doc_display_zz_catalog_write
  BEFORE INSERT OR UPDATE OF source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,discovery_hash,discovery,state ON doc_display_jobs_v1
  FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_job_write();
