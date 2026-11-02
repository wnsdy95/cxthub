ALTER TABLE memory_capture_headers
    ADD COLUMN capture_version integer,
    ADD COLUMN graph_epoch text COLLATE "C",
    ADD COLUMN graph_sequence bigint,
    ADD COLUMN graph_anchor_id text COLLATE "C",
    ADD CONSTRAINT memory_capture_graph_metadata CHECK (
        (capture_version IS NULL AND graph_epoch IS NULL AND graph_sequence IS NULL AND graph_anchor_id IS NULL)
        OR (capture_version IS NOT NULL AND capture_version IN (1,2)
            AND graph_epoch IS NOT NULL AND octet_length(graph_epoch)=36
            AND graph_sequence IS NOT NULL AND graph_sequence>=0
            AND ((capture_version=1 AND graph_anchor_id IS NULL)
                OR (capture_version=2 AND graph_anchor_id IS NOT NULL
                    AND graph_anchor_id ~ '^sha256:[0-9a-f]{64}$' AND graph_anchor_id<>capture_id)))),
    ADD CONSTRAINT memory_capture_graph_anchor FOREIGN KEY (repo_id,graph_anchor_id)
        REFERENCES memory_capture_headers(repo_id,capture_id) ON DELETE NO ACTION;

CREATE INDEX memory_capture_graph_anchors
    ON memory_capture_headers(repo_id,graph_epoch,graph_sequence DESC,capture_id)
    INCLUDE (expires_at)
    WHERE capture_version=1 AND graph_anchor_id IS NULL;

CREATE INDEX memory_capture_graph_dependents
    ON memory_capture_headers(repo_id,graph_anchor_id)
    WHERE graph_anchor_id IS NOT NULL;

CREATE FUNCTION cxt_memory_capture_part_sealed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_repo text;
    target_capture text;
BEGIN
    IF TG_OP='DELETE' THEN
        target_repo := OLD.repo_id;
        target_capture := OLD.capture_id;
    ELSE
        target_repo := NEW.repo_id;
        target_capture := NEW.capture_id;
    END IF;
    IF EXISTS (SELECT 1 FROM memory_capture_headers WHERE repo_id=target_repo AND capture_id=target_capture) THEN
        RAISE EXCEPTION 'memory capture parts are sealed' USING ERRCODE='23514';
    END IF;
    IF TG_OP='DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_capture_part_sealed BEFORE INSERT OR DELETE ON memory_capture_parts
FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_part_sealed();

CREATE FUNCTION cxt_memory_capture_part_indexed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM memory_capture_parts WHERE repo_id=NEW.repo_id AND capture_id=NEW.capture_id AND ordinal=NEW.ordinal)
        AND NOT EXISTS (SELECT 1 FROM memory_capture_headers
            WHERE repo_id=NEW.repo_id AND capture_id=NEW.capture_id AND NEW.ordinal<part_count) THEN
        RAISE EXCEPTION 'memory capture part is not indexed' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_capture_part_indexed AFTER INSERT ON memory_capture_parts
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_part_indexed();

CREATE FUNCTION cxt_memory_capture_graph_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    capture_header jsonb;
    graph_checkpoint jsonb;
    anchor_header jsonb;
    anchor_checkpoint jsonb;
    anchor_expires timestamptz;
    reference_checkpoint jsonb;
    actual_parts bigint;
    actual_body bigint;
    actual_keys bigint;
    reference_count bigint;
    invalid boolean;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM memory_capture_headers WHERE repo_id=NEW.repo_id AND capture_id=NEW.capture_id) THEN
        RETURN NULL;
    END IF;
    capture_header := convert_from(NEW.header,'UTF8')::jsonb;
    graph_checkpoint := capture_header->'generation'->'graph';
    IF capture_header->>'rules' IS DISTINCT FROM '1'
        OR graph_checkpoint->>'repo_id' IS DISTINCT FROM NEW.repo_id
        OR graph_checkpoint->>'scope' IS DISTINCT FROM 'repository-graph-v1'
        OR graph_checkpoint->>'version' IS DISTINCT FROM '1'
        OR (NEW.capture_version IS NULL AND capture_header->>'version' IS DISTINCT FROM '1')
        OR (NEW.capture_version IS NOT NULL AND (
            capture_header->>'version' IS DISTINCT FROM NEW.capture_version::text
            OR graph_checkpoint->>'epoch' IS DISTINCT FROM NEW.graph_epoch
            OR graph_checkpoint->>'sequence' IS DISTINCT FROM NEW.graph_sequence::text)) THEN
        RAISE EXCEPTION 'memory capture graph metadata mismatch' USING ERRCODE='23514';
    END IF;
    SELECT count(*),COALESCE(sum(octet_length(body)),0),COALESCE(sum(octet_length(kind)::bigint+octet_length(entity_key)),0),
        count(*) FILTER (WHERE kind='graph_ref'),
        COALESCE(bool_or(ordinal<>expected_ordinal OR body IS NULL
            OR (capture_header->>'version'='1' AND (kind='graph_ref' OR kind LIKE 'graph_delta:%'))
            OR (capture_header->>'version'='2' AND kind LIKE 'graph:%')),false)
        INTO actual_parts,actual_body,actual_keys,reference_count,invalid
        FROM (SELECT part.*,block.body,row_number() OVER (ORDER BY part.ordinal)-1 AS expected_ordinal
            FROM memory_capture_parts part LEFT JOIN memory_capture_blocks block
                ON block.repo_id=part.repo_id AND block.block_hash=part.block_hash
            WHERE part.repo_id=NEW.repo_id AND part.capture_id=NEW.capture_id
            ORDER BY part.ordinal LIMIT 110001) measured;
    IF invalid OR actual_parts<>NEW.part_count OR actual_body<>NEW.body_bytes OR actual_keys<>NEW.key_bytes THEN
        RAISE EXCEPTION 'memory capture is incomplete' USING ERRCODE='23514';
    END IF;
    IF NEW.capture_version=2 THEN
        SELECT convert_from(header,'UTF8')::jsonb,expires_at INTO anchor_header,anchor_expires
            FROM memory_capture_headers WHERE repo_id=NEW.repo_id AND capture_id=NEW.graph_anchor_id
                AND complete AND graph_anchor_id IS NULL AND (capture_version IS NULL OR capture_version=1)
                AND expires_at>clock_timestamp() FOR KEY SHARE;
        anchor_checkpoint := anchor_header->'generation'->'graph';
        IF reference_count<>1 OR anchor_header->>'version' IS DISTINCT FROM '1'
            OR anchor_header->>'rules' IS DISTINCT FROM capture_header->>'rules'
            OR anchor_checkpoint-'sequence' IS DISTINCT FROM graph_checkpoint-'sequence'
            OR (anchor_checkpoint->>'sequence')::bigint>NEW.graph_sequence
            OR NEW.expires_at>anchor_expires THEN
            RAISE EXCEPTION 'memory capture anchor mismatch' USING ERRCODE='23514';
        END IF;
        SELECT convert_from(block.body,'UTF8')::jsonb INTO reference_checkpoint
            FROM memory_capture_parts part JOIN memory_capture_blocks block
                ON block.repo_id=part.repo_id AND block.block_hash=part.block_hash
            WHERE part.repo_id=NEW.repo_id AND part.capture_id=NEW.capture_id AND part.kind='graph_ref'
                AND part.entity_key=NEW.graph_anchor_id AND part.source_seq=(anchor_checkpoint->>'sequence')::bigint
                AND 'sha256:' || encode(sha256(block.body),'hex')=part.block_hash;
        IF reference_checkpoint IS DISTINCT FROM anchor_checkpoint THEN
            RAISE EXCEPTION 'memory capture graph reference mismatch' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER memory_capture_graph_binding AFTER INSERT ON memory_capture_headers
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cxt_memory_capture_graph_binding();
