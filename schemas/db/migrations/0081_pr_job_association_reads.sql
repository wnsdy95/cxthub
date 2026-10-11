CREATE INDEX pr_history_checkpoints_job_diagnostics ON pr_history_checkpoints (repo_id, number, origin, provider_repository_id);
CREATE INDEX pr_participations_job_diagnostics ON pr_participations (repo_id, number, id);
CREATE INDEX pr_contributions_job_diagnostics ON pr_contributions (repo_id, (payload->'pr'->>'number'), id);
