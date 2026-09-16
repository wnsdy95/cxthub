-- Retained customer payload only; derived indexes and DB overhead are excluded.
-- A namespace is the billing/dedup boundary, including all its workspaces.
CREATE TABLE storage_accounts (
 namespace_id text PRIMARY KEY REFERENCES namespaces(id),
 plan text CHECK(plan IN ('free','team','enterprise')),
 included_bytes bigint NOT NULL DEFAULT 0 CHECK(included_bytes>=0),
 payg boolean NOT NULL DEFAULT false,
 max_bytes bigint CHECK(max_bytes>=0),
 grace_bytes bigint NOT NULL DEFAULT 0 CHECK(grace_bytes>=0),
 grace_until timestamptz,
 reconciled_at timestamptz,
 reconcile_after timestamptz NOT NULL DEFAULT '-infinity',
 current_bytes bigint NOT NULL DEFAULT 0 CHECK(current_bytes>=0),
 policy_revision bigint NOT NULL DEFAULT 0,
 changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_tx bigint NOT NULL DEFAULT 0,
 tx_start_bytes bigint NOT NULL DEFAULT 0,
 tx_reconcile boolean NOT NULL DEFAULT false,
 CHECK(plan IS DISTINCT FROM 'enterprise' OR (included_bytes=53687091200 AND payg)),
 CHECK(plan IS DISTINCT FROM 'free' OR NOT payg)
);
CREATE TABLE storage_usage_objects (
 namespace_id text NOT NULL REFERENCES storage_accounts(namespace_id),
 object_key text NOT NULL,
 bytes bigint NOT NULL CHECK(bytes>0),
 PRIMARY KEY(namespace_id,object_key)
);
CREATE INDEX storage_usage_objects_key ON storage_usage_objects(object_key);
CREATE TABLE storage_usage_ledger (
 seq bigserial PRIMARY KEY,
 namespace_id text NOT NULL REFERENCES storage_accounts(namespace_id),
 delta_bytes bigint NOT NULL,
 bytes_after bigint NOT NULL,
 included_bytes bigint NOT NULL,
 payg boolean NOT NULL,
 plan text,
 reason text NOT NULL,
 object_key text NOT NULL DEFAULT '',
 occurred_at timestamptz NOT NULL
);
CREATE INDEX storage_usage_ledger_time ON storage_usage_ledger(namespace_id,occurred_at,seq);
CREATE TABLE storage_policy_operations (
 id text PRIMARY KEY,
 namespace_id text NOT NULL REFERENCES storage_accounts(namespace_id),
 payload jsonb NOT NULL,
 revision bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION cxt_immutable_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'storage audit records are immutable'; END $$;
CREATE TRIGGER immutable_storage_ledger BEFORE UPDATE OR DELETE ON storage_usage_ledger FOR EACH ROW EXECUTE FUNCTION cxt_immutable_usage();
CREATE TRIGGER immutable_storage_policy BEFORE UPDATE OR DELETE ON storage_policy_operations FOR EACH ROW EXECUTE FUNCTION cxt_immutable_usage();

CREATE VIEW storage_source_objects AS
 SELECT w.owner_namespace_id AS namespace_id, 'blob:'||b.hash AS object_key, max(octet_length(b.bytes))::bigint AS bytes
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash JOIN repos r ON r.id=rb.repo_id JOIN workspaces w ON w.id=r.workspace_id
 WHERE w.owner_namespace_id IS NOT NULL GROUP BY w.owner_namespace_id,b.hash
 UNION ALL
 SELECT w.owner_namespace_id,'settings:'||s.hash,max(octet_length(s.data::text))::bigint
 FROM settings_objects s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id
 WHERE w.owner_namespace_id IS NOT NULL GROUP BY w.owner_namespace_id,s.hash
 UNION ALL
 SELECT w.owner_namespace_id,'secrets:'||s.repo_id,octet_length(s.data::text)::bigint
 FROM repo_secrets s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE w.owner_namespace_id IS NOT NULL
 UNION ALL
 SELECT w.owner_namespace_id,'defaults:'||s.repo_id||':'||s.kind,octet_length(s.data::text)::bigint
 FROM repo_settings s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE w.owner_namespace_id IS NOT NULL;

CREATE FUNCTION cxt_storage_apply(ns text, obj text, amount bigint, why text, reconcile boolean DEFAULT false) RETURNS void LANGUAGE plpgsql AS $$
DECLARE a storage_accounts; old_bytes bigint; delta bigint; stamp timestamptz;
BEGIN
 IF ns IS NULL THEN RETURN; END IF;
 INSERT INTO storage_accounts(namespace_id) VALUES(ns) ON CONFLICT DO NOTHING;
 SELECT * INTO a FROM storage_accounts WHERE namespace_id=ns FOR UPDATE;
 SELECT bytes INTO old_bytes FROM storage_usage_objects WHERE namespace_id=ns AND object_key=obj;
 delta:=amount-coalesce(old_bytes,0);
 IF delta=0 THEN RETURN; END IF;
 stamp:=CASE WHEN a.last_tx=txid_current() THEN a.changed_at ELSE clock_timestamp() END;
 UPDATE storage_accounts SET current_bytes=current_bytes+delta,changed_at=stamp,last_tx=txid_current(),
 tx_start_bytes=CASE WHEN a.last_tx=txid_current() THEN a.tx_start_bytes ELSE a.current_bytes END,
 tx_reconcile=CASE WHEN a.last_tx=txid_current() THEN a.tx_reconcile OR reconcile ELSE reconcile END WHERE namespace_id=ns;
 IF amount=0 THEN DELETE FROM storage_usage_objects WHERE namespace_id=ns AND object_key=obj;
 ELSE INSERT INTO storage_usage_objects VALUES(ns,obj,amount) ON CONFLICT(namespace_id,object_key) DO UPDATE SET bytes=excluded.bytes; END IF;
 INSERT INTO storage_usage_ledger(namespace_id,delta_bytes,bytes_after,included_bytes,payg,plan,reason,object_key,occurred_at)
 VALUES(ns,delta,a.current_bytes+delta,a.included_bytes,a.payg,a.plan,why,obj,stamp);
END $$;

-- Validate the committed net growth, not intermediate manifest/chunk conversion.
CREATE FUNCTION cxt_storage_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE a storage_accounts; cap bigint;
BEGIN
 SELECT * INTO a FROM storage_accounts WHERE namespace_id=NEW.namespace_id;
 IF a.plan IS NULL OR a.tx_reconcile OR a.current_bytes<=a.tx_start_bytes THEN RETURN NULL; END IF;
 cap:=CASE WHEN a.payg THEN a.max_bytes ELSE least(a.included_bytes,coalesce(a.max_bytes,a.included_bytes)) END;
 IF cap IS NULL THEN RETURN NULL; END IF;
 IF a.grace_until>clock_timestamp() THEN cap:=cap+a.grace_bytes; END IF;
 IF a.current_bytes>cap THEN RAISE EXCEPTION USING ERRCODE='CXT01', MESSAGE='storage limit reached; existing context remains readable'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER storage_quota AFTER UPDATE ON storage_accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
 WHEN (NEW.current_bytes IS DISTINCT FROM OLD.current_bytes) EXECUTE FUNCTION cxt_storage_check();

CREATE FUNCTION cxt_storage_refresh_key(ns text,obj text,why text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE size bigint;
BEGIN
 IF ns IS NULL THEN RETURN; END IF;
 INSERT INTO storage_accounts(namespace_id) VALUES(ns) ON CONFLICT DO NOTHING;
 PERFORM 1 FROM storage_accounts WHERE namespace_id=ns FOR UPDATE;
 IF obj LIKE 'blob:%' THEN
 SELECT octet_length(b.bytes) INTO size FROM blobs b WHERE b.hash=substr(obj,6) AND EXISTS (SELECT 1 FROM repo_blobs rb JOIN repos r ON r.id=rb.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE rb.hash=b.hash AND w.owner_namespace_id=ns);
 ELSIF obj LIKE 'settings:%' THEN
 SELECT max(octet_length(s.data::text)) INTO size FROM settings_objects s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE s.hash=substr(obj,10) AND w.owner_namespace_id=ns;
 ELSIF obj LIKE 'defaults:%' THEN
 SELECT octet_length(s.data::text) INTO size FROM repo_settings s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE s.repo_id=substr(obj,10,71) AND s.kind=split_part(obj,':',4) AND w.owner_namespace_id=ns;
 ELSE SELECT octet_length(s.data::text) INTO size FROM repo_secrets s JOIN repos r ON r.id=s.repo_id JOIN workspaces w ON w.id=r.workspace_id WHERE s.repo_id=substr(obj,9) AND w.owner_namespace_id=ns;
 END IF;
 size:=coalesce(size,0);
 -- Re-encoding an already owned immutable object must not disable reads.
 PERFORM cxt_storage_apply(ns,obj,size,why,why='payload.representation');
END $$;
CREATE FUNCTION cxt_storage_payload_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ns text; obj text; rid text;
BEGIN
 rid:=CASE WHEN TG_OP='DELETE' THEN OLD.repo_id ELSE NEW.repo_id END;
 SELECT w.owner_namespace_id INTO ns FROM repos r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=rid FOR SHARE OF r,w;
 IF TG_TABLE_NAME='repo_blobs' THEN obj:='blob:'||CASE WHEN TG_OP='DELETE' THEN OLD.hash ELSE NEW.hash END;
 ELSIF TG_TABLE_NAME='settings_objects' THEN obj:='settings:'||CASE WHEN TG_OP='DELETE' THEN OLD.hash ELSE NEW.hash END;
 ELSIF TG_TABLE_NAME='repo_settings' THEN obj:='defaults:'||rid||':'||CASE WHEN TG_OP='DELETE' THEN OLD.kind ELSE NEW.kind END;
 ELSE obj:='secrets:'||rid; END IF;
 PERFORM cxt_storage_refresh_key(ns,obj,'payload.'||lower(TG_OP));
 RETURN NULL;
END $$;
CREATE TRIGGER meter_repo_blobs AFTER INSERT OR DELETE ON repo_blobs FOR EACH ROW EXECUTE FUNCTION cxt_storage_payload_change();
CREATE TRIGGER meter_settings AFTER INSERT OR UPDATE OR DELETE ON settings_objects FOR EACH ROW EXECUTE FUNCTION cxt_storage_payload_change();
CREATE TRIGGER meter_defaults AFTER INSERT OR UPDATE OR DELETE ON repo_settings FOR EACH ROW EXECUTE FUNCTION cxt_storage_payload_change();
CREATE TRIGGER meter_secrets AFTER INSERT OR UPDATE OR DELETE ON repo_secrets FOR EACH ROW EXECUTE FUNCTION cxt_storage_payload_change();
CREATE FUNCTION cxt_storage_blob_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ns text;
BEGIN
 FOR ns IN SELECT namespace_id FROM storage_usage_objects WHERE object_key='blob:'||NEW.hash ORDER BY namespace_id LOOP
 PERFORM cxt_storage_refresh_key(ns,'blob:'||NEW.hash,'payload.representation'); END LOOP;
 RETURN NULL;
END $$;
CREATE TRIGGER meter_blob_reencoding AFTER UPDATE OF bytes ON blobs FOR EACH ROW EXECUTE FUNCTION cxt_storage_blob_change();

CREATE FUNCTION cxt_storage_reconcile(ns text, corrective boolean DEFAULT true) RETURNS void LANGUAGE plpgsql AS $$
DECLARE row record; a storage_accounts; actual bigint; stamp timestamptz;
BEGIN
 INSERT INTO storage_accounts(namespace_id) VALUES(ns) ON CONFLICT DO NOTHING;
 PERFORM 1 FROM storage_accounts WHERE namespace_id=ns FOR UPDATE;
 FOR row IN SELECT coalesce(src.object_key,old.object_key) AS key,coalesce(src.bytes,0) AS bytes
 FROM (SELECT object_key,bytes FROM storage_source_objects WHERE namespace_id=ns) src
 FULL JOIN (SELECT object_key,bytes FROM storage_usage_objects WHERE namespace_id=ns) old USING(object_key)
 WHERE src.bytes IS DISTINCT FROM old.bytes ORDER BY key LOOP
 PERFORM cxt_storage_apply(ns,row.key,row.bytes,CASE WHEN corrective THEN 'reconcile' ELSE 'ownership.changed' END,corrective);
 END LOOP;
 SELECT * INTO a FROM storage_accounts WHERE namespace_id=ns;
 SELECT coalesce(sum(bytes),0) INTO actual FROM storage_usage_objects WHERE namespace_id=ns;
 IF actual<>a.current_bytes THEN
 stamp:=CASE WHEN a.last_tx=txid_current() THEN a.changed_at ELSE clock_timestamp() END;
 UPDATE storage_accounts SET current_bytes=actual,changed_at=stamp,last_tx=txid_current(),tx_reconcile=corrective,
 tx_start_bytes=CASE WHEN a.last_tx=txid_current() THEN a.tx_start_bytes ELSE a.current_bytes END WHERE namespace_id=ns;
 INSERT INTO storage_usage_ledger(namespace_id,delta_bytes,bytes_after,included_bytes,payg,plan,reason,occurred_at)
 VALUES(ns,actual-a.current_bytes,actual,a.included_bytes,a.payg,a.plan,'reconcile.balance',stamp);
 END IF;
 UPDATE storage_accounts SET reconciled_at=clock_timestamp(),reconcile_after=clock_timestamp()+interval '24 hours' WHERE namespace_id=ns;
END $$;
CREATE FUNCTION cxt_storage_ownership_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ns text;
BEGIN
 -- Rare ownership/attachment mutations reconcile affected namespaces atomically.
 -- Namespace IDs sort the lock order for transfers.
 IF TG_TABLE_NAME='repos' THEN
 FOR ns IN SELECT DISTINCT owner_namespace_id FROM workspaces WHERE id IN (OLD.workspace_id,NEW.workspace_id) AND owner_namespace_id IS NOT NULL ORDER BY owner_namespace_id LOOP
 PERFORM cxt_storage_reconcile(ns,false); END LOOP;
 ELSE
 FOR ns IN SELECT DISTINCT unnest(ARRAY[OLD.owner_namespace_id,NEW.owner_namespace_id]) AS id ORDER BY id LOOP
 IF ns IS NOT NULL THEN PERFORM cxt_storage_reconcile(ns,false); END IF; END LOOP;
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER meter_repo_attachment AFTER UPDATE OF workspace_id ON repos FOR EACH ROW WHEN (OLD.workspace_id IS DISTINCT FROM NEW.workspace_id) EXECUTE FUNCTION cxt_storage_ownership_change();
CREATE TRIGGER meter_workspace_owner AFTER UPDATE OF owner_namespace_id ON workspaces FOR EACH ROW WHEN (OLD.owner_namespace_id IS DISTINCT FROM NEW.owner_namespace_id) EXECUTE FUNCTION cxt_storage_ownership_change();

-- Existing retained bytes start metering now; do not invent past consumption or
-- activate unapproved prices/entitlements during migration.
DO $$ DECLARE ns text; BEGIN FOR ns IN SELECT id FROM namespaces ORDER BY id LOOP PERFORM cxt_storage_reconcile(ns); END LOOP; END $$;
