CREATE OR REPLACE FUNCTION cxt_notify_memory_read_wake(p_repo text,p_objects boolean) RETURNS void LANGUAGE plpgsql AS $$
DECLARE stamp bigint;
BEGIN
    IF NOT EXISTS(SELECT 1 FROM memory_read_work_scopes WHERE repo_id=p_repo AND enabled) THEN RETURN; END IF;
    stamp=nextval('memory_read_wake_seq');
    INSERT INTO memory_read_wakes(repo_id,revision,object_revision,writer_xid,next_attempt)
    VALUES(p_repo,stamp,CASE WHEN p_objects THEN stamp ELSE 0 END,pg_current_xact_id(),clock_timestamp())
    ON CONFLICT(repo_id) DO UPDATE SET
        (revision,object_revision)=(
            WITH bumped AS MATERIALIZED (
                SELECT CASE WHEN memory_read_wakes.writer_xid IS DISTINCT FROM pg_current_xact_id()
                    THEN nextval('memory_read_wake_seq') ELSE memory_read_wakes.revision END AS revision
            )
            SELECT bumped.revision,
                CASE WHEN p_objects THEN bumped.revision ELSE memory_read_wakes.object_revision END
            FROM bumped
        ),
        writer_xid=pg_current_xact_id(),next_attempt=clock_timestamp(),attempts=0,reason='';
END
$$;
