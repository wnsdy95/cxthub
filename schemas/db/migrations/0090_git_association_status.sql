CREATE INDEX git_contribution_admissions_publication
    ON git_contribution_admissions (repo_id, origin, (payload->>'publication_id'), id);

CREATE INDEX git_contribution_supplements_origin_id
    ON git_contribution_supplements (repo_id, origin, id);
