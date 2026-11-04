ALTER TABLE doc_range_generations_v2 ADD COLUMN generation_id uuid NOT NULL DEFAULT gen_random_uuid()
  CHECK (generation_id <> '00000000-0000-0000-0000-000000000000'::uuid);

CREATE FUNCTION cxt_preserve_doc_range_generation_id() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.generation_id := OLD.generation_id;
  RETURN NEW;
END;
$$;

CREATE TRIGGER preserve_doc_range_generation_id
  BEFORE UPDATE ON doc_range_generations_v2
  FOR EACH ROW EXECUTE FUNCTION cxt_preserve_doc_range_generation_id();
