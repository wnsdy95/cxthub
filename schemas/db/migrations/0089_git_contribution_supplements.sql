CREATE FUNCTION git_contribution_supplement_id(repo TEXT, contribution TEXT, publication TEXT)
RETURNS TEXT LANGUAGE SQL IMMUTABLE STRICT AS $$
    SELECT encode(digest(convert_to(
        '{"version":1,"repo_id":"' || repo || '","contribution_id":"' || contribution || '","publication_id":"' || publication || '"}',
        'UTF8'), 'sha256'), 'hex')
$$;

CREATE TABLE git_contribution_supplement_anchors (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    contribution_id TEXT NOT NULL,
    proof_id TEXT NOT NULL,
    PRIMARY KEY (repo_id, contribution_id),
    UNIQUE (repo_id, contribution_id, proof_id),
    FOREIGN KEY (repo_id, contribution_id) REFERENCES git_contributions(repo_id, id) ON DELETE CASCADE,
    FOREIGN KEY (repo_id, proof_id) REFERENCES git_contribution_proofs(repo_id, id) ON DELETE CASCADE
);

INSERT INTO git_contribution_supplement_anchors(repo_id, contribution_id, proof_id)
SELECT repo_id, contribution_id, MIN(id)
FROM git_contribution_proofs GROUP BY repo_id, contribution_id;

CREATE INDEX git_contributions_supplement_match ON git_contributions (
    repo_id, (payload->>'git_origin'), (payload->>'source_branch_id'),
    (payload->>'capture'), (payload->>'original_head'), id
);

CREATE INDEX git_association_publications_supplement_match ON git_association_publications (
    repo_id, origin, (payload->>'branch_id'), (payload->>'target'), (payload->>'git_after'), publication_id
) WHERE payload->>'memory_pinned'='true' AND payload->>'source'=payload->>'target';

CREATE INDEX git_contribution_memories_publication ON git_contribution_memories (repo_id, contribution_id, publication_id);

CREATE TABLE git_contribution_supplements (
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    origin TEXT NOT NULL CHECK (origin <> ''),
    contribution_id TEXT NOT NULL,
    publication_id TEXT NOT NULL,
    proof_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version >= 0),
    state TEXT NOT NULL CHECK (state IN ('waiting', 'retrying', 'processing', 'completed', 'attention')),
    next_attempt TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    payload JSONB NOT NULL,
    PRIMARY KEY (repo_id, contribution_id, publication_id),
    UNIQUE (repo_id, id),
    CHECK (id=git_contribution_supplement_id(repo_id, contribution_id, publication_id)),
    FOREIGN KEY (repo_id, contribution_id, proof_id) REFERENCES git_contribution_supplement_anchors(repo_id, contribution_id, proof_id) ON DELETE CASCADE,
    FOREIGN KEY (repo_id, origin, publication_id) REFERENCES git_association_publications(repo_id, origin, publication_id) ON DELETE CASCADE
);

CREATE INDEX git_contribution_supplements_due ON git_contribution_supplements (repo_id, next_attempt, id)
    WHERE state IN ('waiting', 'retrying', 'processing');

CREATE VIEW git_contribution_supplement_candidates AS
SELECT contribution.repo_id, git_contribution_supplement_id(contribution.repo_id, contribution.id, publication.publication_id) AS id,
    publication.origin, contribution.id AS contribution_id, publication.publication_id, anchor.proof_id,
    0::bigint AS version, 'waiting'::text AS state, CURRENT_TIMESTAMP AS next_attempt,
    TIMESTAMPTZ '0001-01-01 00:00:00+00' AS lease_until,
    jsonb_build_object(
        'id', git_contribution_supplement_id(contribution.repo_id, contribution.id, publication.publication_id),
        'repo_id', contribution.repo_id,
        'git_origin', publication.origin,
        'contribution_id', contribution.id,
        'publication_id', publication.publication_id,
        'proof_id', anchor.proof_id,
        'version', '0',
        'state', 'waiting',
        'attempts', 0,
        'next_attempt', CURRENT_TIMESTAMP,
        'lease_until', '0001-01-01T00:00:00Z'
    ) AS payload
FROM git_contribution_supplement_anchors anchor
JOIN git_contributions contribution ON contribution.repo_id=anchor.repo_id AND contribution.id=anchor.contribution_id
JOIN git_contribution_proofs proof ON proof.repo_id=anchor.repo_id AND proof.id=anchor.proof_id AND proof.contribution_id=contribution.id
JOIN git_association_publications publication ON publication.repo_id=contribution.repo_id
    AND publication.origin=contribution.payload->>'git_origin'
    AND publication.payload->>'branch_id'=contribution.payload->>'source_branch_id'
    AND publication.payload->>'target'=contribution.payload->>'capture'
    AND publication.payload->>'source'=publication.payload->>'target'
    AND publication.payload->>'git_after'=contribution.payload->>'original_head'
    AND publication.payload->>'memory_pinned'='true'
JOIN context_history accepted ON accepted.repo_id=publication.repo_id AND accepted.id=publication.publication_id AND accepted.event=publication.payload
JOIN repos repo ON repo.id=contribution.repo_id AND repo.git_remote_url=publication.origin
WHERE NOT EXISTS (
    SELECT 1 FROM git_contribution_memories memory
    WHERE memory.repo_id=contribution.repo_id AND memory.contribution_id=contribution.id AND memory.publication_id=publication.publication_id
);

INSERT INTO git_contribution_supplements(repo_id,id,origin,contribution_id,publication_id,proof_id,version,state,next_attempt,lease_until,payload)
SELECT repo_id,id,origin,contribution_id,publication_id,proof_id,version,state,next_attempt,lease_until,payload
FROM git_contribution_supplement_candidates;
