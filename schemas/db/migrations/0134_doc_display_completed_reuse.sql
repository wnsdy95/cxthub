CREATE INDEX doc_display_jobs_v1_completed_reuse
  ON doc_display_jobs_v1(source_hash,repo_id,doc_hash,event_index)
  WHERE profile='public-event-display-v1' AND worker_protocol=5 AND state='completed'
    AND preparation_hash IS NOT NULL AND preparation IS NOT NULL;

CREATE INDEX doc_display_jobs_v1_completed_string_reuse
  ON doc_display_jobs_v1(source_hash,repo_id,doc_hash,event_index)
  WHERE profile='tool-result-string-v1' AND worker_protocol=2 AND state='completed'
    AND checkpoint_hash IS NOT NULL AND checkpoint IS NOT NULL;
