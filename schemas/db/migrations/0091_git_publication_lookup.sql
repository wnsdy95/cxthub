CREATE INDEX context_history_git_publication_witness
    ON context_history (
        repo_id,
        (COALESCE(event->>'branch_id','')),
        (digest(COALESCE(event->>'branch',''),'sha256')),
        (digest(COALESCE(event->>'local_branch',''),'sha256')),
        (COALESCE(event->>'worktree_id','')),
        (COALESCE(event->>'target','')),
        (COALESCE(event->>'git_after','')),
        id
    )
    WHERE event->>'kind' IN ('birth','attach','position','advance');

CREATE INDEX context_history_git_branch_bindings
    ON context_history (repo_id, id)
    WHERE event->>'kind' IN ('birth','orphan','rename','archive');

CREATE INDEX context_history_git_head_owner
    ON context_history (repo_id, (COALESCE(event->>'git_after','')), (COALESCE(event->>'branch_id','')), id)
    WHERE event->>'kind' IN ('birth','attach','position','advance')
        AND COALESCE(event->>'target','')<>'';
