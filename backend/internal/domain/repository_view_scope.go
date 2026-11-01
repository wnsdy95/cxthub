package domain

import "context"

func ValidateRepositoryViewScope(ctx context.Context, repo ContentHash, view RepositoryView) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateContentHash(repo); err != nil {
		return err
	}
	check := func(owner ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if owner != repo {
			return ErrIntegrity
		}
		return nil
	}
	for _, ref := range view.Refs {
		if err := check(ref.RepoID); err != nil {
			return err
		}
	}
	for _, snapshot := range view.Snapshots {
		if err := check(snapshot.RepoID); err != nil {
			return err
		}
	}
	for _, event := range view.History {
		if err := check(ContentHash(event.RepoID)); err != nil {
			return err
		}
	}
	for _, pending := range view.Pending {
		if err := check(pending.RepoID); err != nil {
			return err
		}
	}
	for _, unsync := range view.Unsync {
		if err := check(unsync.RepoID); err != nil {
			return err
		}
	}
	for _, archive := range view.ArchivedSessions {
		if err := check(archive.RepoID); err != nil {
			return err
		}
	}
	return ctx.Err()
}
